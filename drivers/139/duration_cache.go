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
 * 139 视频时长持久化缓存
 *
 * 存储位置：data/139_duration_cache.json
 * 结构：{ "fileId": 1234.567, ... }   // 单位秒
 *
 * 特性：
 *   - 进程内 sync.Map 做一级缓存，避免频繁读文件
 *   - 写操作 200ms 防抖，批量合并写入
 *   - 文件损坏时自动忽略，不影响主流程
 *   - 进程退出时 Flush 一次
 * ============================================================ */

const durationCacheFile = "data/139_duration_cache.json"

var (
	persistCache     sync.Map      // fileID(string) -> duration(float64)
	persistLoaded    bool
	persistLoadMu    sync.Mutex
	persistDirty     bool
	persistDirtyMu   sync.Mutex
	persistFlushOnce sync.Once
	persistFlushCh   = make(chan struct{}, 1)
)

// 启动时调用一次，从磁盘加载缓存
func loadDurationCacheOnce() {
	persistLoadMu.Lock()
	defer persistLoadMu.Unlock()
	if persistLoaded {
		return
	}
	persistLoaded = true

	data, err := os.ReadFile(durationCacheFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("[139] duration cache read failed: %v", err)
		}
		return
	}

	var m map[string]float64
	if err := json.Unmarshal(data, &m); err != nil {
		log.Warnf("[139] duration cache parse failed (ignored): %v", err)
		return
	}
	for k, v := range m {
		if v > 0 {
			persistCache.Store(k, v)
		}
	}
	log.Infof("[139] duration cache loaded: %d entries", len(m))

	// 启动后台 flusher
	persistFlushOnce.Do(func() {
		go durationCacheFlusher()
	})
}

// 后台 flusher：收到信号后 200ms 合并写入一次
func durationCacheFlusher() {
	for range persistFlushCh {
		time.Sleep(200 * time.Millisecond)

		// 清空信号（合并多次写请求）
		for {
			select {
			case <-persistFlushCh:
				continue
			default:
			}
			break
		}

		flushDurationCacheToDisk()
	}
}

func flushDurationCacheToDisk() {
	persistDirtyMu.Lock()
	if !persistDirty {
		persistDirtyMu.Unlock()
		return
	}
	persistDirty = false
	persistDirtyMu.Unlock()

	// 收集所有条目
	m := make(map[string]float64)
	persistCache.Range(func(k, v interface{}) bool {
		ks, ok1 := k.(string)
		vf, ok2 := v.(float64)
		if ok1 && ok2 && vf > 0 {
			m[ks] = vf
		}
		return true
	})

	data, err := json.Marshal(m)
	if err != nil {
		log.Warnf("[139] duration cache marshal failed: %v", err)
		return
	}

	// 确保目录存在
	dir := filepath.Dir(durationCacheFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warnf("[139] duration cache mkdir failed: %v", err)
		return
	}

	// 原子写：先写临时文件，再 rename
	tmp := durationCacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Warnf("[139] duration cache write tmp failed: %v", err)
		return
	}
	if err := os.Rename(tmp, durationCacheFile); err != nil {
		log.Warnf("[139] duration cache rename failed: %v", err)
	}
}

// 读缓存（优先内存）
func getPersistDuration(fileID string) float64 {
	if fileID == "" {
		return 0
	}
	if v, ok := persistCache.Load(fileID); ok {
		if f, ok := v.(float64); ok && f > 0 {
			return f
		}
	}
	return 0
}

// 写缓存（内存 + 异步落盘）
func setPersistDuration(fileID string, dur float64) {
	if fileID == "" || !(dur > 0) {
		return
	}
	persistCache.Store(fileID, dur)

	persistDirtyMu.Lock()
	persistDirty = true
	persistDirtyMu.Unlock()

	select {
	case persistFlushCh <- struct{}{}:
	default:
	}
}