package op

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/singleflight"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

// 归档内部提取（/ae，压缩包内图片预览）此前每次请求都会重新打开底层存储、
// 重新解析压缩包并解压成员，且响应带 no-store，前端切一张图会发出多次 /ae，
// 在 crypt + 移动云盘的链路上尤其慢。
//
// 这里把「已解压出来的单个文件」完整缓存到 TempDir 下的临时文件：
//   - 同一张图片只解压一次，后续请求直接打开缓存文件；
//   - 用 singleflight 保证并发请求只解压一次；
//   - TTL + 总容量上限 + 过期清理，避免临时文件无限堆积。
//
// 缓存只服务于「内容不经常变化」的预览场景；压缩包被替换后最多 TTL 时间
// 内仍可能读到旧内容，这与归档 meta/list 的缓存行为一致。
const (
	extractFileTTL        = 30 * time.Minute
	extractFileSweepEvery = time.Minute
	extractFilePrefix     = "openlist-extract-"
)

// 缓存占用磁盘上限，超过后按最旧优先淘汰。用变量以便在单元测试中缩小。
// 图片预加载量调大后需要更大的缓存，否则旧图刚预加载就会被淘汰。
var extractFileMaxBytes int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

type extractedFile struct {
	path      string
	size      int64
	expiresAt time.Time
}

type extractFileStore struct {
	mu      sync.Mutex
	entries map[string]*extractedFile
	bytes   int64
	started bool
}

var extractFiles = extractFileStore{entries: make(map[string]*extractedFile)}

// extractFileKey 生成缓存键。只使用驱动自己的挂载路径、归档路径、成员路径和
// 密码，不含请求头等易变信息，保证同一张图片的多次请求命中同一条缓存。
func extractFileKey(storage driver.Driver, path string, args model.ArchiveInnerArgs) string {
	identity := fmt.Sprintf("%s|%s|%s|%s",
		storage.GetStorage().MountPath,
		utils.FixAndCleanPath(path),
		utils.FixAndCleanPath(args.InnerPath),
		args.Password,
	)
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

// extractFileGet 返回仍有效的缓存条目，顺手清理过期项。
func extractFileGet(key string) (*extractedFile, bool) {
	extractFiles.mu.Lock()
	defer extractFiles.mu.Unlock()
	e, ok := extractFiles.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		removeExtractFileLocked(key)
		return nil, false
	}
	return e, true
}

// extractFilePut 写入缓存并触发容量清理。
func extractFilePut(key, path string, size int64) {
	extractFiles.startSweeper()
	extractFiles.mu.Lock()
	defer extractFiles.mu.Unlock()
	if old, ok := extractFiles.entries[key]; ok {
		extractFiles.bytes -= old.size
		_ = os.Remove(old.path)
	}
	extractFiles.entries[key] = &extractedFile{
		path:      path,
		size:      size,
		expiresAt: time.Now().Add(extractFileTTL),
	}
	extractFiles.bytes += size
	extractFiles.enforceLimitLocked()
}

func extractFileDelete(key string) {
	extractFiles.mu.Lock()
	defer extractFiles.mu.Unlock()
	removeExtractFileLocked(key)
}

// removeExtractFileLocked 调用方必须持有 extractFiles.mu。
func removeExtractFileLocked(key string) {
	if e, ok := extractFiles.entries[key]; ok {
		extractFiles.bytes -= e.size
		_ = os.Remove(e.path)
		delete(extractFiles.entries, key)
	}
}

// enforceLimitLocked 调用方必须持有 extractFiles.mu。按过期时间从旧到新淘汰，
// 直到总占用不超过上限。
func (s *extractFileStore) enforceLimitLocked() {
	for s.bytes > extractFileMaxBytes && len(s.entries) > 0 {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range s.entries {
			if first || e.expiresAt.Before(oldest) {
				oldestKey, oldest, first = k, e.expiresAt, false
			}
		}
		removeExtractFileLocked(oldestKey)
	}
}

func (s *extractFileStore) startSweeper() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go func() {
		ticker := time.NewTicker(extractFileSweepEvery)
		defer ticker.Stop()
		for range ticker.C {
			s.sweep()
		}
	}()
}

func (s *extractFileStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			removeExtractFileLocked(k)
		}
	}
}

// cachedExtract 是 singleflight 的结果：一份落到磁盘的完整解压内容。
type cachedExtract struct {
	path string
	size int64
}

var extractFileG singleflight.Group[*cachedExtract]

// openCachedExtract 命中缓存时打开一份全新的只读 fd；每次调用返回独立的
// *os.File，调用方负责 Close，彼此之间偏移互不影响。
func openCachedExtract(e *extractedFile) (io.ReadCloser, int64, error) {
	f, err := os.Open(e.path)
	if err != nil {
		return nil, 0, err
	}
	return f, e.size, nil
}

// extractToTempFile 真正执行一次解压，把内容完整写入临时文件。
func extractToTempFile(ctx context.Context, storage driver.Driver, path string, args model.ArchiveInnerArgs) (*cachedExtract, error) {
	_, t, ss, err := GetArchiveToolAndStream(ctx, storage, path, args.LinkArgs)
	if err != nil {
		return nil, err
	}
	closeStreams := func() {
		for _, s := range ss {
			if e := s.Close(); e != nil {
				log.Debugf("close archive stream: %v", e)
			}
		}
	}
	rc, _, err := t.Extract(ss, args)
	if err != nil {
		closeStreams()
		return nil, err
	}

	tempDir := ""
	if conf.Conf != nil {
		tempDir = conf.Conf.TempDir
	}
	tmp, err := os.CreateTemp(tempDir, extractFilePrefix)
	if err != nil {
		_ = rc.Close()
		closeStreams()
		return nil, err
	}
	tmpPath := tmp.Name()
	n, copyErr := utils.CopyWithBuffer(tmp, rc)
	closeErr := tmp.Close()
	_ = rc.Close()
	closeStreams()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		if copyErr != nil {
			return nil, copyErr
		}
		return nil, closeErr
	}
	return &cachedExtract{path: tmpPath, size: n}, nil
}

// getCachedExtract 是带 singleflight 的缓存读取入口：命中直接返回磁盘路径，
// 未命中则由单个 goroutine 解压落盘，其它并发请求等待同一份结果。
func getCachedExtract(ctx context.Context, key string, storage driver.Driver, path string, args model.ArchiveInnerArgs) (*cachedExtract, error) {
	if e, ok := extractFileGet(key); ok {
		return &cachedExtract{path: e.path, size: e.size}, nil
	}
	result, err, _ := extractFileG.Do(key, func() (*cachedExtract, error) {
		if e, ok := extractFileGet(key); ok {
			return &cachedExtract{path: e.path, size: e.size}, nil
		}
		ce, err := extractToTempFile(ctx, storage, path, args)
		if err != nil {
			return nil, err
		}
		extractFilePut(key, ce.path, ce.size)
		return ce, nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("empty extracted cache result")
	}
	return result, nil
}
