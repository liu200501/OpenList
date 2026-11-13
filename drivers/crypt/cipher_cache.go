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
//
// 这份密文同时是唯一的数据副本：加密在任务入队前就完成了（见 PrepareUpload），
// 明文流不再被缓存，所以上传期间只有一份密文在磁盘上，而不是「明文占内存 + 密文占磁盘」。
const (
	cipherCacheTTL = 24 * time.Hour
	cipherCacheTag = ".openlist-crypt-cache"
	// 加密落盘阶段占总进度的 30%，剩下 70% 留给真正的上传
	cipherStageRatio = 0.3
	// 后台清理间隔。光靠上传时顺手扫有个洞：实例长期没有新上传，
	// 过期密文会一直占着 temp，所以再加一条定时清理。
	cipherCacheSweepInterval = time.Hour
)

type cipherCacheEntry struct {
	path      string
	size      int64
	sha256    string
	expiresAt time.Time
}

var cipherCache = struct {
	sync.Mutex
	items map[string]*cipherCacheEntry
}{items: make(map[string]*cipherCacheEntry)}

// cipherCacheKey 只要求「同一次上传任务在重试时算出同一个 key」，所以 identity
// 用驱动自己视角的路径即可（crypt 侧路径 / 底层实际路径不会混用）。
func cipherCacheKey(storageID uint, identity string, size int64, mtime time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d|%d", storageID, identity, size, mtime.UnixNano())))
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

// janitorOnce 保证后台清理协程只起一次；懒启动，避免包初始化阶段就依赖运行环境。
var janitorOnce sync.Once

func startCipherCacheJanitor() {
	janitorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(cipherCacheSweepInterval)
			defer ticker.Stop()
			for range ticker.C {
				sweepExpired()
			}
		}()
	})
}

// sweepExpired 清理过期缓存，避免进程长期运行后临时文件堆积。
// 由上传流程顺手调用，同时也有后台 ticker 兜底（见 startCipherCacheJanitor）。
func sweepExpired() {
	startCipherCacheJanitor()
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

// openCiphertext 打开一份已落盘的密文，命中会顺延它的 TTL。
func openCiphertext(key string) (*os.File, int64, string, bool) {
	cipherCache.Lock()
	defer cipherCache.Unlock()
	e, ok := cipherCache.items[key]
	if !ok {
		return nil, 0, "", false
	}
	if time.Now().After(e.expiresAt) {
		_ = os.Remove(e.path)
		delete(cipherCache.items, key)
		return nil, 0, "", false
	}
	f, err := os.Open(e.path)
	if err != nil {
		delete(cipherCache.items, key)
		return nil, 0, "", false
	}
	e.expiresAt = time.Now().Add(cipherCacheTTL)
	return f, e.size, e.sha256, true
}

// materializeCiphertext 把明文流一次性加密落盘并登记进缓存，返回临时文件路径、密文大小
// 与密文的 SHA256。写完立刻关闭句柄；后续每次上传（含重试）都通过 openCiphertext 重新打开，
// 这样暂停时不会留下被占用的文件句柄（Windows 上删不掉）。
//
// 顺带算 SHA256 是白送的：字节本来就要过一遍手，写盘才是瓶颈（实测约 359MB/s），
// 而 SHA256 有硬件加速，两者不叠加耗时。上层 139 拿到这个值就不用再全量读一遍求摘要。
func (d *Crypt) materializeCiphertext(
	ctx context.Context,
	key string,
	identity string,
	streamer model.FileStreamer,
	up driver.UpdateProgress,
) (string, int64, string, error) {
	wrapped, err := d.cipher.EncryptData(streamer)
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to EncryptData: %w", err)
	}
	tmp, err := os.CreateTemp(conf.Conf.TempDir, "openlist-crypt-*"+cipherCacheTag)
	if err != nil {
		return "", 0, "", fmt.Errorf("failed to create ciphertext cache: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	expect := d.cipher.EncryptedSize(streamer.GetSize())
	buf := make([]byte, 1<<20)
	hasher := sha256.New()
	var written int64
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", 0, "", ctxErr
		}
		n, readErr := wrapped.Read(buf)
		if n > 0 {
			if _, writeErr := tmp.Write(buf[:n]); writeErr != nil {
				return "", 0, "", writeErr
			}
			// 必须先确认落盘成功再喂 hasher，否则摘要可能对应一份不完整的文件
			hasher.Write(buf[:n])
			written += int64(n)
			if up != nil && expect > 0 {
				up(float64(written) / float64(expect) * cipherStageRatio * 100)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return "", 0, "", readErr
		}
	}
	if err := tmp.Close(); err != nil {
		return "", 0, "", err
	}
	committed = true

	sum := hex.EncodeToString(hasher.Sum(nil))
	cipherCache.Lock()
	if old, exist := cipherCache.items[key]; exist {
		// 同一目标被并发重传时，旧的那份密文没人再引用了，先删掉再登记
		_ = os.Remove(old.path)
	}
	cipherCache.items[key] = &cipherCacheEntry{
		path:      tmp.Name(),
		size:      written,
		sha256:    sum,
		expiresAt: time.Now().Add(cipherCacheTTL),
	}
	cipherCache.Unlock()
	log.Debugf("crypt: cached ciphertext of [%s] (%d bytes, sha256=%s) for resumable upload", identity, written, sum)
	return tmp.Name(), written, sum, nil
}

func ciphertextCleanup(f *os.File, key string) func(success bool) {
	return func(success bool) {
		_ = f.Close()
		if success {
			dropCiphertext(key)
		}
	}
}

// stableCiphertext 返回一份可以跨多次上传重放的密文。
// cleanup 必须在底层 Put 结束后调用；success 为 true 时删除缓存文件，
// 否则保留下来供下次续传使用。
//
// 这是「入队前没预加密」时的兜底路径（比如非 as-task 的直接上传仍带 KeepStreamKey）。
// PutAsTask 场景下驱动会走 PrepareUpload 提前把密文准备好，不会到这里。
func (d *Crypt) stableCiphertext(
	ctx context.Context,
	storageID uint,
	remoteDir string,
	name string,
	streamer model.FileStreamer,
	up driver.UpdateProgress,
) (reader io.Reader, size int64, sum string, cleanup func(success bool), err error) {
	remotePath := stdpath.Join(remoteDir, name)
	key := cipherCacheKey(storageID, remotePath, streamer.GetSize(), streamer.ModTime())

	if f, cachedSize, cachedSum, ok := openCiphertext(key); ok {
		log.Debugf("crypt: reuse cached ciphertext of [%s] (%d bytes)", remotePath, cachedSize)
		return f, cachedSize, cachedSum, ciphertextCleanup(f, key), nil
	}

	sweepExpired()
	if _, _, _, err = d.materializeCiphertext(ctx, key, remotePath, streamer, up); err != nil {
		return nil, 0, "", nil, err
	}
	f, cachedSize, cachedSum, ok := openCiphertext(key)
	if !ok {
		return nil, 0, "", nil, fmt.Errorf("ciphertext cache of [%s] disappeared right after being written", remotePath)
	}
	return f, cachedSize, cachedSum, ciphertextCleanup(f, key), nil
}
