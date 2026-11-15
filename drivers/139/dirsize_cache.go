package _139

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

/* ============================================================
 * 139 个人云文件夹大小持久化缓存
 *
 * 存储位置：data/139_dirsize_cache.json
 * 结构：{ "folderId": 123456789, ... }   // 直接子文件大小之和，字节
 *
 * 每次进入目录会用最新列表重新计算并强制覆盖；文件损坏时忽略，不影响主流程。
 * ============================================================ */

const dirSizeCacheFile = "data/139_dirsize_cache.json"

var (
	dirSizeCache     sync.Map // folderID(string) -> dirSize(int64)
	dirSizeLoaded    bool
	dirSizeLoadMu    sync.Mutex
	dirSizeDirty     bool
	dirSizeDirtyMu   sync.Mutex
	dirSizeFlushOnce sync.Once
	dirSizeFlushCh   = make(chan struct{}, 1)
)

func loadDirSizeCacheOnce() {
	dirSizeLoadMu.Lock()
	defer dirSizeLoadMu.Unlock()
	if dirSizeLoaded {
		return
	}
	dirSizeLoaded = true

	data, err := os.ReadFile(dirSizeCacheFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("[139] dirsize cache read failed: %v", err)
		}
		return
	}
	var m map[string]int64
	if err := json.Unmarshal(data, &m); err != nil {
		log.Warnf("[139] dirsize cache parse failed (ignored): %v", err)
		return
	}
	for k, v := range m {
		if v > 0 {
			dirSizeCache.Store(k, v)
		}
	}
	log.Infof("[139] dirsize cache loaded: %d entries", len(m))

	dirSizeFlushOnce.Do(func() {
		go dirSizeCacheFlusher()
	})
}

func dirSizeCacheFlusher() {
	for range dirSizeFlushCh {
		time.Sleep(200 * time.Millisecond)
		for {
			select {
			case <-dirSizeFlushCh:
				continue
			default:
			}
			break
		}
		flushDirSizeCacheToDisk()
	}
}

func flushDirSizeCacheToDisk() {
	dirSizeDirtyMu.Lock()
	if !dirSizeDirty {
		dirSizeDirtyMu.Unlock()
		return
	}
	dirSizeDirty = false
	dirSizeDirtyMu.Unlock()

	m := make(map[string]int64)
	dirSizeCache.Range(func(k, v interface{}) bool {
		ks, ok1 := k.(string)
		vi, ok2 := v.(int64)
		if ok1 && ok2 && vi > 0 {
			m[ks] = vi
		}
		return true
	})

	data, err := json.Marshal(m)
	if err != nil {
		log.Warnf("[139] dirsize cache marshal failed: %v", err)
		return
	}
	dir := filepath.Dir(dirSizeCacheFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warnf("[139] dirsize cache mkdir failed: %v", err)
		return
	}
	tmp := dirSizeCacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Warnf("[139] dirsize cache write tmp failed: %v", err)
		return
	}
	if err := os.Rename(tmp, dirSizeCacheFile); err != nil {
		log.Warnf("[139] dirsize cache rename failed: %v", err)
	}
}

// getDirSize 读取缓存；未点进过的目录返回 0。
func getDirSize(folderID string) int64 {
	if folderID == "" {
		return 0
	}
	if v, ok := dirSizeCache.Load(folderID); ok {
		if s, ok := v.(int64); ok && s > 0 {
			return s
		}
	}
	return 0
}

// setDirSize 强制覆盖写入（含 0，表示空目录），并触发异步落盘。
func setDirSize(folderID string, size int64) {
	if folderID == "" || size < 0 {
		return
	}
	dirSizeCache.Store(folderID, size)

	dirSizeDirtyMu.Lock()
	dirSizeDirty = true
	dirSizeDirtyMu.Unlock()

	select {
	case dirSizeFlushCh <- struct{}{}:
	default:
	}
}
