// Package indexwatch 实现“按驱动/按路径”主动维护搜索索引。
//
// 背景：search 包现有的自动更新依赖 op.List 触发的 ObjsUpdateHook，只有
// 用户真的在 OpenList 里浏览目录时才会更新；网盘侧（如中国移动云盘）的
// 变更不会主动通知 OpenList。本包对少数配置的目录做受控扫描，绕开全局
// auto_update_index 开关，直接调用 search.UpdateForced 维护索引。
//
// 支持的驱动模式：
//   - Local：用 fsnotify 监听真实目录，事件驱动（debounce 500ms），近乎实时。
//   - 139Yun / Crypt：按 interval 定时递归扫描指定目录（网盘无事件推送能力）。
//
// Crypt 是虚拟加密层，底层事件是密文路径，映射回明文需要完整 nameMap，
// 因此 Crypt 统一走定时扫描，由 OpenList 自己解密后再更新索引。
package indexwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/search"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

// Rule 是一条监听规则，来自设置项 index_watch_rules 的 JSON 数组。
type Rule struct {
	Enabled  bool   `json:"enabled"`
	Driver   string `json:"driver"` // 139Yun / Local / Crypt，留空表示不限
	Path     string `json:"path"`   // OpenList 挂载虚拟路径
	Interval int    `json:"interval"`
	Depth    int    `json:"depth"` // 递归深度，0 表示只看当前目录
}

// watcher 是一条规则对应的运行时状态。
type watcher struct {
	rule      Rule
	ctx       context.Context
	cancel    context.CancelFunc
	running   bool
	mu        sync.Mutex
	failCount int
	// Local 规则的 fsnotify 监听（懒启动，仅 local 模式使用）
	local *localWatcher
}

var (
	managerMu sync.Mutex
	watchers  []*watcher
	started   bool
	scanSem   = make(chan struct{}, 2) // 全局并发闸门

	// 设置 hook 在 db 保存之前触发，此时 setting.Get 仍是旧值。
	// 用 pending 记录 hook 带来的新值，Reload 时优先采用。
	pendingMu      sync.Mutex
	pendingEnable  *bool
	pendingRules   *string
)

// Start 在存储加载完成后启动监听；重复调用安全。
func Start() {
	managerMu.Lock()
	defer managerMu.Unlock()
	if started {
		return
	}
	started = true
	reloadLocked()
}

// Reload 从设置重新加载规则（先停旧、再建新）。
func Reload() {
	managerMu.Lock()
	defer managerMu.Unlock()
	if !started {
		// 尚未启动（例如初始化时设置 hook 触发）：消费并丢弃 pending，
		// 避免旧值残留到真正启动后。启动时会从 setting 读最新值。
		pendingMu.Lock()
		pendingEnable = nil
		pendingRules = nil
		pendingMu.Unlock()
		return
	}
	reloadLocked()
}

// Stop 停止所有监听。
func Stop() {
	managerMu.Lock()
	defer managerMu.Unlock()
	for _, w := range watchers {
		w.cancel()
	}
	watchers = nil
}

func reloadLocked() {
	for _, w := range watchers {
		w.cancel()
	}
	watchers = nil

	enable := setting.GetBool(conf.IndexWatchEnable)
	rulesRaw := setting.GetStr(conf.IndexWatchRules)
	pendingMu.Lock()
	if pendingEnable != nil {
		enable = *pendingEnable
	}
	if pendingRules != nil {
		rulesRaw = *pendingRules
	}
	pendingEnable = nil
	pendingRules = nil
	pendingMu.Unlock()

	if !enable {
		log.Debugf("[indexwatch] disabled")
		return
	}
	if !search.Enabled() {
		// 不在这里退出：用户可能在运行中把 search_index 从 none 改为 database。
		// UpdateForced 内部会检查实例，未就绪时静默跳过。
		log.Warnf("[indexwatch] enabled but search index is not available yet: set search_index to database or meilisearch and build the index")
	}
	rules := parseRules(rulesRaw)
	for i := range rules {
		w := newWatcher(rules[i])
		watchers = append(watchers, w)
		go w.loop()
	}
	log.Infof("[indexwatch] started %d watcher(s)", len(watchers))
}

func parseRules(raw string) []Rule {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var rules []Rule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		log.Errorf("[indexwatch] invalid index_watch_rules JSON: %v", err)
		return nil
	}
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if !r.Enabled || strings.TrimSpace(r.Path) == "" {
			continue
		}
		r.Path = utils.FixAndCleanPath(r.Path)
		if r.Interval <= 0 {
			r.Interval = 60
		}
		if r.Depth < 0 {
			r.Depth = 0
		}
		out = append(out, r)
	}
	return out
}

func newWatcher(rule Rule) *watcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &watcher{
		rule:   rule,
		ctx:    ctx,
		cancel: cancel,
	}
}

// loop 按规则间隔循环扫描；首次启动先做一次，避免等一个 interval。
func (w *watcher) loop() {
	w.tick()
	ticker := time.NewTicker(time.Duration(w.rule.Interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.tick()
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *watcher) tick() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		log.Debugf("[indexwatch] previous round still running, skip: %s", w.rule.Path)
		return
	}
	w.running = true
	fail := w.failCount
	w.mu.Unlock()

	// 失败退避：1x,2x,4x,8x（最大 30 分钟），避免被网盘风控。
	if fail > 0 {
		backoff := time.Duration(w.rule.Interval) * time.Second
		for i := 0; i < fail && i < 4; i++ {
			backoff *= 2
		}
		if backoff > 30*time.Minute {
			backoff = 30 * time.Minute
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-w.ctx.Done():
			timer.Stop()
			w.mu.Lock()
			w.running = false
			w.mu.Unlock()
			return
		}
	}

	select {
	case scanSem <- struct{}{}:
		if err := w.scan(); err != nil {
			w.mu.Lock()
			w.failCount++
			w.mu.Unlock()
			log.Warnf("[indexwatch] scan %s failed: %v", w.rule.Path, err)
		} else {
			w.mu.Lock()
			w.failCount = 0
			w.mu.Unlock()
		}
		<-scanSem
	case <-w.ctx.Done():
	}

	w.mu.Lock()
	w.running = false
	w.mu.Unlock()
}

// scan 根据规则类型执行扫描。
// Local 走 fsnotify 常驻监听（在 startLocal 中启动一次），这里只做一次
// 初始对齐；139Yun/Crypt(139Yun) 走定时递归扫描。
func (w *watcher) scan() error {
	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Minute)
	defer cancel()

	mode, err := w.resolveMode()
	if err != nil {
		return err
	}
	if mode == "local" {
		// 首次启动时确保 fsnotify 监听已就位，然后做一次全量对齐。
		w.ensureLocalWatcher()
		return w.baselineLocal(ctx)
	}
	return w.scanRecursive(ctx)
}

// resolveMode 判断规则实际采用哪种扫描方式；Crypt 解析其底层存储类型。
func (w *watcher) resolveMode() (string, error) {
	storage, _, err := op.GetStorageAndActualPath(w.rule.Path)
	if err != nil {
		return "", fmt.Errorf("resolve storage for %s: %w", w.rule.Path, err)
	}
	driverName := strings.TrimSpace(w.rule.Driver)
	if driverName == "" {
		driverName = storage.Config().Name
	} else {
		// 用户填写的规则做大小写归一化，兼容 local / LOCAL / 139yun 等写法。
		lower := strings.ToLower(driverName)
		switch lower {
		case "local":
			driverName = "Local"
		case "crypt":
			driverName = "Crypt"
		case "139yun":
			driverName = "139Yun"
		}
	}
	switch driverName {
	case "Local":
		return "local", nil
	case "139Yun":
		return "poll", nil
	case "Crypt":
		// Crypt 是虚拟加密层：底层文件事件拿到的是密文路径，映射回明文路径
		// 需要完整 nameMap，容易出错。因此 Crypt 统一走定时扫描（OpenList
		// 内部会解密为明文路径后再更新索引），不直接挂 fsnotify。
		return "poll", nil
	default:
		return "poll", nil
	}
}

// scanRecursive 对 139Yun / Crypt(139Yun) 做定时递归扫描。
func (w *watcher) scanRecursive(ctx context.Context) error {
	admin, err := op.GetAdmin()
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, conf.UserKey, admin)
	return w.walkAndUpdate(ctx, w.rule.Path, w.rule.Depth)
}

func (w *watcher) walkAndUpdate(ctx context.Context, dir string, depth int) error {
	select {
	case <-w.ctx.Done():
		return nil
	default:
	}
	if isSkipDriver(dir) {
		return nil
	}
	objs, err := fs.List(ctx, dir, &fs.ListArgs{Refresh: true, SkipHook: true, NoLog: true})
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	search.UpdateForced(ctx, dir, objs)
	if depth <= 0 {
		return nil
	}
	for _, obj := range objs {
		if obj.IsDir() {
			child := path.Join(dir, obj.GetName())
			if err := w.walkAndUpdate(ctx, child, depth-1); err != nil {
				log.Debugf("[indexwatch] skip child %s: %v", child, err)
			}
		}
	}
	return nil
}

// baselineLocal 在 Local 规则启动时做一次全量对齐：递归列目录写索引，
// 并填充快照，确保之后 fsnotify 事件能正确识别"变化"。
func (w *watcher) baselineLocal(ctx context.Context) error {
	admin, err := op.GetAdmin()
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, conf.UserKey, admin)
	var collectErr error
	var collect func(dir string, depth int)
	collect = func(dir string, depth int) {
		select {
		case <-w.ctx.Done():
			return
		default:
		}
		objs, err := fs.List(ctx, dir, &fs.ListArgs{Refresh: true, SkipHook: true, NoLog: true})
		if err != nil {
			collectErr = err
			return
		}
		search.UpdateForced(ctx, dir, objs)
		if depth <= 0 {
			return
		}
		for _, obj := range objs {
			if obj.IsDir() {
				collect(path.Join(dir, obj.GetName()), depth-1)
			}
		}
	}
	collect(w.rule.Path, w.rule.Depth)
	return collectErr
}

// isSkipDriver 跳过虚拟/循环类驱动。
func isSkipDriver(dir string) bool {
	storage, _, err := op.GetStorageAndActualPath(dir)
	if err != nil {
		return false
	}
	switch storage.Config().Name {
	case "Virtual", "OpenList", "Alias":
		return true
	default:
		return false
	}
}

func init() {
	// 存储加载完成后启动。
	go func() {
		<-conf.StoragesLoadSignal()
		Start()
	}()
	// 设置变更时，用 hook 提供的新值重载（此时 setting.Get 仍是旧值）。
	op.RegisterSettingItemHook(conf.IndexWatchEnable, func(item *model.SettingItem) error {
		b := item.Value == "true" || item.Value == "1"
		pendingMu.Lock()
		pendingEnable = &b
		pendingMu.Unlock()
		Reload()
		return nil
	})
	op.RegisterSettingItemHook(conf.IndexWatchRules, func(item *model.SettingItem) error {
		v := item.Value
		pendingMu.Lock()
		pendingRules = &v
		pendingMu.Unlock()
		Reload()
		return nil
	})
}
