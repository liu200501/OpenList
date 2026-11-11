package crypt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	stdpath "path"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	log "github.com/sirupsen/logrus"
)

// rclone crypt 给每个文件随机生成一个 nonce 并写进密文头部，所以同一份明文
// 加密两次得到的字节完全不同。而 139 这类存储的断点续传是按字节续写的：
// 密文一变，之前上传的分片全部作废，complete 时的 SHA256 校验会直接报
// 「文件内容摘要值不匹配」。
//
// 因此对「底层存储支持续传」的场景，把密文完整落到临时文件后复用，
// 保证暂停 / 重试时喂给底层的是同一份字节。
const (
	cipherCacheTTL = 24 * time.Hour
	cipherCacheTag = ".openlist-crypt-cache"
	// 加密落盘阶段占总进度的 30%，剩下 70% 留给真正的上传
	cipherStageRatio = 0.3
)

type cipherCacheEntry struct {
	path      string
	size      int64
	expiresAt time.Time
}

var cipherCache = struct {
	sync.Mutex
	items map[string]*cipherCacheEntry
}{items: make(map[string]*cipherCacheEntry)}

func cipherCacheKey(storageID uint, remotePath string, size int64, mtime time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d|%d", storageID, remotePath, size, mtime.UnixNano())))
	return hex.EncodeToString(sum[:])
}

func dropCiphertext(key string) {
	cipherCache.Lock()
	defer cipherCache.Unlock()
	if e, ok := cipherCache.items[key]; ok {
		_ = os.Remove(e.path)
		delete(cipherCache.items, key)
	}
}

// sweepExpired 顺手清理过期缓存，避免进程长期运行后临时文件堆积。
func sweepExpired() {
	now := time.Now()
	cipherCache.Lock()
	defer cipherCache.Unlock()
	for k, e := range cipherCache.items {
		if now.After(e.expiresAt) {
			_ = os.Remove(e.path)
			delete(cipherCache.items, k)
		}
	}
}

// stableCiphertext 返回一份可以跨多次上传重放的密文。
// cleanup 必须在底层 Put 结束后调用；success 为 true 时删除缓存文件，
// 否则保留下来供下次续传使用。
func (d *Crypt) stableCiphertext(
	ctx context.Context,
	storageID uint,
	remoteDir string,
	name string,
	streamer model.FileStreamer,
	up driver.UpdateProgress,
) (reader io.Reader, size int64, cleanup func(success bool), err error) {
	remotePath := stdpath.Join(remoteDir, name)
	key := cipherCacheKey(storageID, remotePath, streamer.GetSize(), streamer.ModTime())

	cipherCache.Lock()
	if e, ok := cipherCache.items[key]; ok && time.Now().Before(e.expiresAt) {
		if _, statErr := os.Stat(e.path); statErr == nil {
			f, openErr := os.Open(e.path)
			if openErr == nil {
				e.expiresAt = time.Now().Add(cipherCacheTTL)
				cipherCache.Unlock()
				log.Debugf("crypt: reuse cached ciphertext of [%s] (%d bytes)", remotePath, e.size)
				return f, e.size, func(success bool) {
					_ = f.Close()
					if success {
						dropCiphertext(key)
					}
				}, nil
			}
		}
		delete(cipherCache.items, key)
	}
	cipherCache.Unlock()

	sweepExpired()

	wrapped, err := d.cipher.EncryptData(streamer)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to EncryptData: %w", err)
	}
	tmp, err := os.CreateTemp(conf.Conf.TempDir, "openlist-crypt-*"+cipherCacheTag)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to create ciphertext cache: %w", err)
	}
	abort := func(cause error) (io.Reader, int64, func(bool), error) {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, nil, cause
	}

	expect := d.cipher.EncryptedSize(streamer.GetSize())
	buf := make([]byte, 1<<20)
	var written int64
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return abort(ctxErr)
		}
		n, readErr := wrapped.Read(buf)
		if n > 0 {
			if _, writeErr := tmp.Write(buf[:n]); writeErr != nil {
				return abort(writeErr)
			}
			written += int64(n)
			if up != nil && expect > 0 {
				up(float64(written) / float64(expect) * cipherStageRatio * 100)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return abort(readErr)
		}
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return abort(err)
	}

	cipherCache.Lock()
	cipherCache.items[key] = &cipherCacheEntry{
		path:      tmp.Name(),
		size:      written,
		expiresAt: time.Now().Add(cipherCacheTTL),
	}
	cipherCache.Unlock()
	log.Debugf("crypt: cached ciphertext of [%s] (%d bytes) for resumable upload", remotePath, written)

	return tmp, written, func(success bool) {
		_ = tmp.Close()
		if success {
			dropCiphertext(key)
		}
	}, nil
}
