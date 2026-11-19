package indexwatch

import (
	"context"
	"fmt"
	"os"
	stdpath "path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/search"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

// localWatcher 用 fsnotify 监听真实本地目录，事件去重后更新对应的
// OpenList 虚拟路径索引。
type localWatcher struct {
	w       *watcher
	fsw     *fsnotify.Watcher
	rootAbs string // 规则路径对应的真实本地绝对路径

	// realPath(相对 rootAbs) -> 虚拟目录路径
	mu       sync.Mutex
	watched  map[string]bool // 已 add 的真实目录
	dirty    map[string]bool // 待更新的虚拟父目录
	debounce *time.Timer
}

// ensureLocalWatcher 懒启动 fsnotify 监听，重复调用安全。
func (w *watcher) ensureLocalWatcher() {
	w.mu.Lock()
	if w.local != nil {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	lw, err := newLocalWatcher(w)
	if err != nil {
		log.Warnf("[indexwatch][local] cannot watch %s: %v (falling back to periodic baseline only)", w.rule.Path, err)
		return
	}
	w.mu.Lock()
	if w.local != nil {
		w.mu.Unlock()
		lw.fsw.Close()
		return
	}
	w.local = lw
	w.mu.Unlock()
	go lw.run()
	log.Infof("[indexwatch][local] watching %s -> %s", w.rule.Path, lw.rootAbs)
}

func newLocalWatcher(w *watcher) (*localWatcher, error) {
	storage, actualPath, err := op.GetStorageAndActualPath(w.rule.Path)
	if err != nil {
		return nil, err
	}
	if storage.Config().Name != "Local" {
		return nil, fmt.Errorf("rule %s is not a Local storage", w.rule.Path)
	}
	addition, ok := storage.GetAddition().(interface{ GetRootPath() string })
	if !ok {
		return nil, fmt.Errorf("local storage addition has no root path")
	}
	root := addition.GetRootPath()
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		root = abs
	}
	// actualPath 是相对存储根的 OpenList 路径；本地子路径用正斜杠拼接。
	rel := strings.TrimPrefix(utils.FixAndCleanPath(actualPath), "/")
	rootAbs := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(rootAbs)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("watch root %s is not a directory: %v", rootAbs, err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	lw := &localWatcher{
		w:       w,
		fsw:     fsw,
		rootAbs: rootAbs,
		watched: make(map[string]bool),
		dirty:   make(map[string]bool),
	}
	// 递归添加监听目录。
	if err := lw.addTree(rootAbs, w.rule.Depth); err != nil {
		fsw.Close()
		return nil, err
	}
	return lw, nil
}

// addTree 递归 addWatch，depth 为 0 时只监听当前目录。
func (lw *localWatcher) addTree(dir string, depth int) error {
	if err := lw.addWatch(dir); err != nil {
		return err
	}
	if depth <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // 单个子目录失败不影响整体
	}
	for _, e := range entries {
		if e.IsDir() {
			child := filepath.Join(dir, e.Name())
			if err := lw.addTree(child, depth-1); err != nil {
				log.Debugf("[indexwatch][local] skip watch %s: %v", child, err)
			}
		}
	}
	return nil
}

func (lw *localWatcher) addWatch(dir string) error {
	lw.mu.Lock()
	if lw.watched[dir] {
		lw.mu.Unlock()
		return nil
	}
	lw.mu.Unlock()
	if err := lw.fsw.Add(dir); err != nil {
		return err
	}
	lw.mu.Lock()
	lw.watched[dir] = true
	lw.mu.Unlock()
	return nil
}

// run 消费事件，debounce 后批量更新索引。
func (lw *localWatcher) run() {
	defer lw.fsw.Close()
	for {
		select {
		case <-lw.w.ctx.Done():
			return
		case event, ok := <-lw.fsw.Events:
			if !ok {
				return
			}
			lw.handleEvent(event)
		case err, ok := <-lw.fsw.Errors:
			if !ok {
				return
			}
			log.Warnf("[indexwatch][local] watcher error: %v", err)
		}
	}
}

func (lw *localWatcher) handleEvent(event fsnotify.Event) {
	abs := event.Name
	// 新目录：加入监听，并把它自己的父目录标脏。
	if event.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			_ = lw.addTree(abs, lw.w.rule.Depth)
		}
	}
	// 事件路径可能是文件也可能是目录；要更新的是它所在的父目录。
	parent := filepath.Dir(abs)
	// 删除事件时目录已不存在，父目录仍在，取上一级即可。
	virtual := lw.toVirtualDir(parent)
	if virtual == "" {
		return
	}
	lw.mu.Lock()
	lw.dirty[virtual] = true
	if lw.debounce != nil {
		lw.debounce.Stop()
	}
	lw.debounce = time.AfterFunc(500*time.Millisecond, lw.flush)
	lw.mu.Unlock()
}

// toVirtualDir 把真实目录转成 OpenList 虚拟路径；不在监听范围内的返回空。
func (lw *localWatcher) toVirtualDir(realDir string) string {
	rel, err := filepath.Rel(lw.rootAbs, realDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return lw.w.rule.Path
	}
	return stdpath.Join(lw.w.rule.Path, rel)
}

// flush 批量更新脏目录的索引。
func (lw *localWatcher) flush() {
	lw.mu.Lock()
	dirs := make([]string, 0, len(lw.dirty))
	for d := range lw.dirty {
		dirs = append(dirs, d)
	}
	lw.dirty = make(map[string]bool)
	lw.mu.Unlock()

	admin, err := op.GetAdmin()
	if err != nil {
		return
	}
	ctx := context.WithValue(lw.w.ctx, conf.UserKey, admin)
	for _, dir := range dirs {
		objs, err := fs.List(ctx, dir, &fs.ListArgs{Refresh: true, SkipHook: true, NoLog: true})
		if err != nil {
			log.Debugf("[indexwatch][local] flush list %s failed: %v", dir, err)
			continue
		}
		search.UpdateForced(ctx, dir, objs)
	}
}
