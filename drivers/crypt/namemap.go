package crypt

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	stdpath "path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/namemap"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	log "github.com/sirupsen/logrus"
)

const (
	nameMapFlushDelay = 2 * time.Second
	nameMapScanCool   = 40 * time.Millisecond
	nameMapMaxDepth   = 64
)

// nameMapEntry 是一条「明文 ↔ 密文」记录。
type nameMapEntry struct {
	Plain     string `json:"plain"`
	Raw       string `json:"raw"`
	PlainPath string `json:"plain_path"`
	RawPath   string `json:"raw_path"`
	Dir       bool   `json:"dir"`
	Size      int64  `json:"size"`
}

type nameMapState struct {
	Mount string                  `json:"mount"`
	Items map[string]nameMapEntry `json:"items"`
}

type nameMapCache struct {
	mu       sync.Mutex
	loaded   bool
	items    map[string]nameMapEntry
	timer    *time.Timer
	scanning bool
}

var (
	nameMapCaches   sync.Map // storageID -> *nameMapCache
	nameMapScanning sync.Map // storageID -> struct{}
)

func (d *Crypt) nameMapCache() *nameMapCache {
	if v, ok := nameMapCaches.Load(d.ID); ok {
		return v.(*nameMapCache)
	}
	v, _ := nameMapCaches.LoadOrStore(d.ID, &nameMapCache{items: map[string]nameMapEntry{}})
	return v.(*nameMapCache)
}

// recordNameMap 在每次列目录时记录映射，增量合并。
// rawDir 是该目录在底层存储里的实际路径（不带挂载前缀），与全量扫描保持同一套 key。
func (d *Crypt) recordNameMap(reqPath, rawDir string, objs []model.Obj) {
	if len(objs) == 0 {
		return
	}
	if reqPath == "" {
		p, ok := d.plainPathByRaw(rawDir)
		if !ok {
			return
		}
		reqPath = p
	}
	entries := make([]nameMapEntry, 0, len(objs))
	for _, o := range objs {
		rawPath := o.GetPath()
		if rawPath == "" {
			continue
		}
		rawName := stdpath.Base(rawPath)
		if rawDir != "" {
			rawPath = stdpath.Join(rawDir, rawName)
		}
		entries = append(entries, nameMapEntry{
			Plain:     o.GetName(),
			Raw:       rawName,
			PlainPath: stdpath.Join(reqPath, o.GetName()),
			RawPath:   rawPath,
			Dir:       o.IsDir(),
			Size:      o.GetSize(),
		})
	}
	if d.mergeNameMap(entries) {
		d.scheduleFlush()
	}
}

// plainPathByRaw 把底层存储的实际路径翻译成明文路径。
// 先查索引（父目录一般已被记录），查不到再逐级解密兜底。
func (d *Crypt) plainPathByRaw(rawPath string) (string, bool) {
	if rawPath == "" {
		return "", false
	}
	rawPath = stdpath.Clean("/" + strings.TrimPrefix(rawPath, "/"))
	c := d.nameMapCache()
	c.mu.Lock()
	if !c.loaded {
		c.load(d.ID)
	}
	e, ok := c.items[rawPath]
	c.mu.Unlock()
	if ok {
		return e.PlainPath, true
	}
	root := d.remoteActualRoot()
	if root == "" {
		return "", false
	}
	root = stdpath.Clean("/" + strings.TrimPrefix(root, "/"))
	if rawPath == root || root == "/" {
		return "/", true
	}
	rel, found := strings.CutPrefix(rawPath, root+"/")
	if !found {
		return "", false
	}
	plain := "/"
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		p, err := d.cipher.DecryptDirName(seg)
		if err != nil {
			return "", false
		}
		plain = stdpath.Join(plain, p)
	}
	return plain, true
}

// remoteActualRoot 返回 crypt 挂载点在底层存储里的实际根路径，如 /加密。
func (d *Crypt) remoteActualRoot() string {
	_, root, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return ""
	}
	return root
}

// remoteActualOf 把虚拟路径（/10086移动/加密/xxx）换成底层实际路径（/加密/xxx）。
func remoteActualOf(virtualPath string) string {
	if virtualPath == "" {
		return ""
	}
	if _, actual, err := op.GetStorageAndActualPath(virtualPath); err == nil && actual != "" {
		return actual
	}
	return virtualPath
}

// nameMapRefresh 记录待刷新的目录，800ms 防抖：批量上传一个目录只会重列一次。
var nameMapRefresh = struct {
	sync.Mutex
	timer map[string]*time.Timer
}{timer: map[string]*time.Timer{}}

// refreshNameMapDir 写操作（新建/上传/改名/移动/删除）后重列该目录，
// 让索引立刻跟上，而不用等用户再点进这个目录。
func (d *Crypt) refreshNameMapDir(virtualDir string) {
	if virtualDir == "" {
		return
	}
	nameMapRefresh.Lock()
	if t, ok := nameMapRefresh.timer[virtualDir]; ok {
		t.Stop()
	}
	nameMapRefresh.timer[virtualDir] = time.AfterFunc(800*time.Millisecond, func() {
		nameMapRefresh.Lock()
		delete(nameMapRefresh.timer, virtualDir)
		nameMapRefresh.Unlock()
		d.doRefreshNameMapDir(virtualDir)
	})
	nameMapRefresh.Unlock()
}

func (d *Crypt) doRefreshNameMapDir(virtualDir string) {
	storage, rawPath, err := op.GetStorageAndActualPath(virtualDir)
	if err != nil {
		log.Debugf("[crypt] index: resolve %s failed: %v", virtualDir, err)
		return
	}
	plain, ok := d.plainPathByRaw(rawPath)
	if !ok {
		log.Debugf("[crypt] index: %s not in index yet, skip refresh", rawPath)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// maxDepth=1：只重列这一层，不递归
	seen := make(map[string]struct{}, 64)
	if _, err := d.scanDir(ctx, storage, rawPath, plain, 0, 1, seen, nil); err != nil {
		log.Debugf("[crypt] index: refresh %s failed: %v", rawPath, err)
		return
	}
	if n := d.pruneNameMapUnder(rawPath, seen); n > 0 {
		log.Infof("[crypt] index of %s: %d entrie(s) gone after %s changed", d.MountPath, n, rawPath)
	}
	d.scheduleFlush()
}

// pruneNameMapUnder 删掉某一层里「这次没列到」的条目（含其整棵子树）。
// 改名/删除后靠它把旧条目清掉，不必等全量重建。
func (d *Crypt) pruneNameMapUnder(rawDir string, seen map[string]struct{}) int {
	rawDir = stdpath.Clean("/" + strings.TrimPrefix(rawDir, "/"))
	c := d.nameMapCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scanning {
		// 全量扫描正在进行时让扫描自己收尾，避免两边互相覆盖
		return 0
	}
	if !c.loaded {
		c.load(d.ID)
	}
	gone := make([]string, 0, 8)
	for k := range c.items {
		if stdpath.Dir(k) != rawDir {
			continue
		}
		if _, ok := seen[k]; !ok {
			gone = append(gone, k)
		}
	}
	removed := 0
	for _, root := range gone {
		prefix := root + "/"
		for k := range c.items {
			if k == root || strings.HasPrefix(k, prefix) {
				delete(c.items, k)
				removed++
			}
		}
	}
	return removed
}


// mergeNameMap 合并条目，返回是否有变化。
func (d *Crypt) mergeNameMap(entries []nameMapEntry) bool {
	c := d.nameMapCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.load(d.ID)
	}
	changed := false
	for _, e := range entries {
		if old, ok := c.items[e.RawPath]; !ok || old != e {
			c.items[e.RawPath] = e
			changed = true
		}
	}
	return changed
}

func (d *Crypt) scheduleFlush() {
	c := d.nameMapCache()
	c.mu.Lock()
	if c.timer != nil || c.scanning {
		c.mu.Unlock()
		return
	}
	c.timer = time.AfterFunc(nameMapFlushDelay, d.flushNameMap)
	c.mu.Unlock()
}

func (c *nameMapCache) load(id uint) {
	c.loaded = true
	b, err := os.ReadFile(namemap.JSONPath(id))
	if err != nil {
		return
	}
	var s nameMapState
	if err := json.Unmarshal(b, &s); err != nil || s.Items == nil {
		return
	}
	c.items = s.Items
}

func (d *Crypt) flushNameMap() {
	c := d.nameMapCache()
	c.mu.Lock()
	items := make([]nameMapEntry, 0, len(c.items))
	for _, e := range c.items {
		items = append(items, e)
	}
	c.timer = nil
	c.mu.Unlock()

	if err := writeNameMap(d.ID, d.MountPath, items); err != nil {
		log.Warnf("[crypt] write name map of storage %s failed: %v", d.MountPath, err)
		return
	}
	log.Infof("[crypt] name map of storage %s updated (%d entries): /cryptmap/%d?sign=%s",
		d.MountPath, len(items), d.ID, sign.Sign(fmt.Sprintf("/cryptmap/%d", d.ID)))
}

func writeNameMap(id uint, mount string, items []nameMapEntry) error {
	if err := namemap.EnsureDir(); err != nil {
		return err
	}
	state := nameMapState{Mount: mount, Items: map[string]nameMapEntry{}}
	for _, e := range items {
		state.Items[e.RawPath] = e
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(namemap.JSONPath(id), data); err != nil {
		return err
	}
	return writeFileAtomic(namemap.HTMLPath(id), []byte(renderNameMap(id, mount, items)))
}

// ensureNameMapIndex 在没有索引文件时启动一次全量扫描。
func (d *Crypt) ensureNameMapIndex() {
	c := d.nameMapCache()
	c.mu.Lock()
	if !c.loaded {
		c.load(d.ID)
	}
	hasIndex := len(c.items) > 0
	c.mu.Unlock()
	if hasIndex {
		return
	}
	d.RescanNameMap()
}

// RescanNameMap 全量遍历存储，把目录与文件的对应关系补进索引。
// 已存在的条目不会被删除，只做合并。
// NameMapPage 用当前索引实时渲染对照页，避免模板改动后磁盘上的 HTML 变陈旧。
func (d *Crypt) NameMapPage() ([]byte, bool) {
	c := d.nameMapCache()
	c.mu.Lock()
	if !c.loaded {
		c.load(d.ID)
	}
	if len(c.items) == 0 {
		c.mu.Unlock()
		return nil, false
	}
	items := make([]nameMapEntry, 0, len(c.items))
	for _, e := range c.items {
		items = append(items, e)
	}
	c.mu.Unlock()
	return []byte(renderNameMap(d.ID, d.MountPath, items)), true
}

func (d *Crypt) RescanNameMap() {
	if _, loaded := nameMapScanning.LoadOrStore(d.ID, struct{}{}); loaded {
		log.Infof("[crypt] index of %s is already running, skip", d.MountPath)
		return
	}
	go func() {
		defer nameMapScanning.Delete(d.ID)
		c := d.nameMapCache()
		c.mu.Lock()
		c.scanning = true
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.scanning = false
			c.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		start := time.Now()
		dirs, errs := d.scanAll(ctx)
		if errs == 0 && ctx.Err() == nil {
			d.pruneNameMap()
		} else if errs > 0 {
			log.Infof("[crypt] index of %s: %d folder(s) failed, skip pruning this round", d.MountPath, errs)
		}
		d.flushNameMap()
		c.mu.Lock()
		total := len(c.items)
		c.mu.Unlock()
		log.Infof("[crypt] index of %s finished: %d folder(s), %d entries, %s",
			d.MountPath, dirs, total, time.Since(start).Round(time.Second))
	}()
}

// scanAll 全量遍历，返回目录数与本轮失败数（失败则不剪枝，宁可留旧条目）。
func (d *Crypt) scanAll(ctx context.Context) (int, int) {
	remoteStorage, remotePath, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		log.Warnf("[crypt] index of %s: resolve remote failed: %v", d.MountPath, err)
		return 0, 1
	}
	seen := make(map[string]struct{}, 4096)
	errs := 0
	dirs, err := d.scanDir(ctx, remoteStorage, remotePath, "/", 0, nameMapMaxDepth, seen, &errs)
	if err != nil {
		return dirs, errs + 1
	}
	// 记下本轮实际看到的条目，供剪枝使用
	nameMapSeen.Store(d.ID, seen)
	return dirs, errs
}

// nameMapSeen 保存最近一次全量扫描看到的条目集合。
var nameMapSeen sync.Map // storageID -> map[string]struct{}

// pruneNameMap 删掉索引里「本轮扫描没看到」的条目，让重建索引真正与云盘对齐。
func (d *Crypt) pruneNameMap() int {
	v, ok := nameMapSeen.Load(d.ID)
	if !ok {
		return 0
	}
	seen := v.(map[string]struct{})
	root := stdpath.Clean("/" + strings.TrimPrefix(d.remoteActualRoot(), "/"))
	c := d.nameMapCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.load(d.ID)
	}
	removed := 0
	for k := range c.items {
		if k != root && !strings.HasPrefix(k, root+"/") {
			continue
		}
		if _, ok := seen[k]; !ok {
			delete(c.items, k)
			removed++
		}
	}
	if removed > 0 {
		log.Infof("[crypt] index of %s: pruned %d stale entrie(s)", d.MountPath, removed)
	}
	return removed
}

// scanDir 直接遍历底层存储，用 cipher 反推明文名。
// maxDepth 控制递归深度（1 = 只列当前层）；seen 不为 nil 时把看到的路径记进去；errs 统计失败目录数。
func (d *Crypt) scanDir(ctx context.Context, storage driver.Driver, rawPath, plainPath string, depth, maxDepth int, seen map[string]struct{}, errs *int) (int, error) {
	if depth >= maxDepth || depth > nameMapMaxDepth {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	objs, err := op.List(ctx, storage, rawPath, model.ListArgs{Refresh: true})
	if err != nil {
		return 0, err
	}
	dirs := 0
	entries := make([]nameMapEntry, 0, len(objs))
	for _, obj := range objs {
		if model.ObjHasMask(obj, model.Virtual) {
			continue
		}
		rawName := model.UnwrapObjName(obj).GetName()
		rawFull := stdpath.Join(rawPath, rawName)
		if seen != nil {
			seen[rawFull] = struct{}{}
		}
		if obj.IsDir() {
			plain, derr := d.cipher.DecryptDirName(rawName)
			if derr != nil {
				continue
			}
			entries = append(entries, nameMapEntry{
				Plain:     plain,
				Raw:       rawName,
				PlainPath: stdpath.Join(plainPath, plain),
				RawPath:   rawFull,
				Dir:       true,
			})
			if depth+1 >= maxDepth {
				continue
			}
			n, serr := d.scanDir(ctx, storage, rawFull, stdpath.Join(plainPath, plain), depth+1, maxDepth, seen, errs)
			dirs += n
			if serr != nil {
				if errs != nil {
					*errs = *errs + 1
				}
				log.Warnf("[crypt] index: list %s failed: %v", rawFull, serr)
			}
			continue
		}
		plain, derr := d.cipher.DecryptFileName(rawName)
		size, serr := d.cipher.DecryptedSize(obj.GetSize())
		if derr != nil || serr != nil {
			continue
		}
		entries = append(entries, nameMapEntry{
			Plain:     plain,
			Raw:       rawName,
			PlainPath: stdpath.Join(plainPath, plain),
			RawPath:   rawFull,
			Size:      size,
		})
	}
	if len(entries) > 0 {
		d.mergeNameMap(entries)
	}
	dirs++
	if dirs%100 == 0 {
		log.Infof("[crypt] index %s: %d folder(s) scanned", d.MountPath, dirs)
	}
	time.Sleep(nameMapScanCool)
	return dirs, nil
}

type nameMapItem struct {
	P string `json:"p"`
	R string `json:"r"`
	D bool   `json:"d"`
	S string `json:"s"`
}

type nameMapPage struct {
	Title      string
	Mount      string
	StorageID  uint
	Updated    string
	TotalDirs  int
	TotalFiles int
	Indexed    bool
	Data       template.JS
}

func renderNameMap(id uint, mount string, items []nameMapEntry) string {
	sort.Slice(items, func(i, j int) bool { return items[i].PlainPath < items[j].PlainPath })
	list := make([]nameMapItem, 0, len(items))
	dirs, files := 0, 0
	for _, e := range items {
		list = append(list, nameMapItem{P: e.PlainPath, R: e.Raw, D: e.Dir, S: humanSize(e.Size)})
		if e.Dir {
			dirs++
		} else {
			files++
		}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		raw = []byte("[]")
	}
	page := nameMapPage{
		Title:      "明文 ↔ 密文",
		Mount:      mount,
		StorageID:  id,
		Updated:    time.Now().Format("2006-01-02 15:04"),
		TotalDirs:  dirs,
		TotalFiles: files,
		Indexed:    true,
		Data:       template.JS(raw),
	}
	var sb strings.Builder
	if err := nameMapTemplate.Execute(&sb, page); err != nil {
		return "<!doctype html><meta charset=\"utf-8\"><p>render failed"
	}
	return sb.String()
}

func humanSize(n int64) string {
	if n <= 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var nameMapTemplate = template.Must(template.New("namemap").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · {{.Mount}}</title>
<style>
:root{color-scheme:light dark}
*{box-sizing:border-box}
body{margin:0;font:14px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;background:#faf9f6;color:#26262a}
.wrap{max-width:1080px;margin:0 auto;padding:20px 20px 60px}
h1{margin:0 0 2px;font-size:17px;font-weight:600}
.meta{color:#6b6b72;font-size:12px}
.bar{display:flex;gap:8px;align-items:center;margin:14px 0 10px;flex-wrap:wrap}
.crumbs{flex:1;display:flex;gap:4px;align-items:center;flex-wrap:wrap;font-size:13px;min-width:240px}
.crumbs a{color:#534ab7;text-decoration:none;cursor:pointer;padding:2px 4px;border-radius:4px}
.crumbs a:hover{background:rgba(83,74,183,.1)}
.crumbs span.sep{color:#b4b4ba}
input{width:260px;padding:7px 11px;border:1px solid rgba(0,0,0,.18);border-radius:8px;background:transparent;color:inherit;font:inherit;outline:none}
input:focus{border-color:#534ab7}
button{padding:7px 12px;border:1px solid rgba(0,0,0,.18);border-radius:8px;background:transparent;color:inherit;font:inherit;cursor:pointer}
button:hover{border-color:#534ab7;color:#534ab7}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid rgba(0,0,0,.07);vertical-align:middle}
th{font-size:12px;color:#6b6b72;font-weight:600}
tr.row{cursor:pointer}
tr.row:hover{background:rgba(83,74,183,.06)}
.name{display:flex;align-items:center;gap:8px}
.ico{width:16px;text-align:center;color:#888780;font-size:12px}
.raw{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px;color:#6b6b72;word-break:break-all}
.dim{color:#6b6b72;font-size:12px}
.empty{padding:24px 0;color:#6b6b72}
.chips{display:flex;gap:6px;justify-content:flex-end}
.chip{padding:2px 9px;border:1px solid rgba(0,0,0,.16);border-radius:6px;font-size:12px;line-height:18px;color:#6b6b72;cursor:pointer;user-select:none;white-space:nowrap}
.chip:hover{border-color:#534ab7;color:#534ab7;background:rgba(83,74,183,.08)}
.chip:active{transform:translateY(1px)}
.toast{position:fixed;left:50%;bottom:24px;transform:translateX(-50%);background:#26262a;color:#fff;padding:8px 14px;border-radius:8px;font-size:13px;opacity:0;transition:opacity .2s;pointer-events:none}
.toast.show{opacity:1}
@media (prefers-color-scheme: dark){
body{background:#1c1c1e;color:#ececee}
.meta,.dim,th,.raw,.ico{color:#a0a0a8}
.chip{color:#a0a0a8;border-color:rgba(255,255,255,.2)}
.chip:hover{border-color:#a9a4f0;color:#c3bef7;background:rgba(169,164,240,.14)}
th,td{border-color:rgba(255,255,255,.1)}
input,button{border-color:rgba(255,255,255,.22)}
input:focus,button:hover{border-color:#a9a4f0}
.crumbs a{color:#c3bef7}
.crumbs a:hover{background:rgba(169,164,240,.15)}
.crumbs span.sep{color:#5f5f66}
tr.row:hover{background:rgba(169,164,240,.08)}
.toast{background:#ececee;color:#1c1c1e}
}
</style>
</head>
<body>
<div class="wrap">
<h1>{{.Title}} · {{.Mount}}</h1>
<div class="meta">存储 ID {{.StorageID}} · 索引更新于 {{.Updated}} · {{.TotalDirs}} 个目录 / {{.TotalFiles}} 个文件</div>

<div class="bar">
<div class="crumbs" id="crumbs"></div>
<input id="q" type="search" placeholder="搜索明文或密文，回车定位" autocomplete="off">
<button id="rescan" title="重新遍历整个存储，把新文件补进索引">重建索引</button>
</div>

<table>
<thead><tr><th style="width:44%">名称</th><th style="width:12%">大小</th><th>云盘上的名字</th><th style="width:120px;text-align:right">复制</th></tr></thead>
<tbody id="body"></tbody>
</table>
<div class="empty" id="empty" style="display:none">这个目录下没有已索引的内容。</div>
</div>
<div class="toast" id="toast"></div>

<script id="nm" type="application/json">{{.Data}}</script>
<script>
(function(){
  var items = JSON.parse(document.getElementById('nm').textContent);
  var byParent = new Map();
  items.forEach(function(it){
    var i = it.p.lastIndexOf('/');
    var parent = i <= 0 ? '/' : it.p.slice(0, i);
    if (!byParent.has(parent)) byParent.set(parent, []);
    byParent.get(parent).push(it);
  });
  byParent.forEach(function(arr){
    arr.sort(function(a,b){
      if (a.d !== b.d) return a.d ? -1 : 1;
      return a.p.localeCompare(b.p, 'zh');
    });
  });

  var cur = '/';
  var body = document.getElementById('body');
  var crumbs = document.getElementById('crumbs');
  var empty = document.getElementById('empty');
  var q = document.getElementById('q');
  var toastEl = document.getElementById('toast');

  function base(p){ var i = p.lastIndexOf('/'); return p.slice(i+1); }
  function parentOf(p){ var i = p.lastIndexOf('/'); return i <= 0 ? '/' : p.slice(0, i); }
  function showToast(t){
    toastEl.textContent = t; toastEl.classList.add('show');
    clearTimeout(showToast._t);
    showToast._t = setTimeout(function(){ toastEl.classList.remove('show'); }, 1800);
  }
  function copy(t, msg){
    if (navigator.clipboard) navigator.clipboard.writeText(t).then(function(){ showToast(msg || '已复制'); }, function(){ showToast(t); });
    else showToast(t);
  }

  // 明文列里显示什么就复制什么；密文同理。
  function plainText(it, showPath){ return showPath ? it.p : base(it.p); }

  function go(p){ cur = p; q.value=''; history.replaceState(null,'','#'+encodeURIComponent(p)); render(); }
  window.addEventListener('hashchange', function(){
    var p = decodeURIComponent(location.hash.slice(1) || '/');
    if (p && p !== cur) { cur = p; render(); }
  });

  function renderCrumbs(){
    crumbs.innerHTML = '';
    var a0 = document.createElement('a');
    a0.textContent = '根目录'; a0.onclick = function(){ go('/'); };
    crumbs.appendChild(a0);
    if (cur === '/') return;
    var segs = cur.split('/').filter(Boolean), acc = '';
    segs.forEach(function(s){
      acc += '/' + s;
      var sep = document.createElement('span'); sep.className='sep'; sep.textContent='›';
      crumbs.appendChild(sep);
      var a = document.createElement('a'); a.textContent = s;
      var target = acc;
      a.onclick = function(){ go(target); };
      crumbs.appendChild(a);
    });
  }

  function chip(label, title, fn){
    var s = document.createElement('span');
    s.className = 'chip'; s.textContent = label; s.title = title;
    s.onclick = function(e){ e.stopPropagation(); fn(); };
    return s;
  }

  function row(it, showPath){
    var tr = document.createElement('tr');
    tr.className = 'row';
    var td1 = document.createElement('td');
    var wrap = document.createElement('div'); wrap.className = 'name';
    var ico = document.createElement('span'); ico.className='ico'; ico.textContent = it.d ? '▸' : '·';
    var nm = document.createElement('span'); nm.textContent = plainText(it, showPath);
    wrap.appendChild(ico); wrap.appendChild(nm);
    td1.appendChild(wrap);
    var td2 = document.createElement('td'); td2.className='dim'; td2.textContent = it.d ? '' : it.s;
    var td3 = document.createElement('td'); td3.className='raw'; td3.textContent = it.r;
    var td4 = document.createElement('td');
    var box = document.createElement('div'); box.className = 'chips';
    box.appendChild(chip('明文', '复制明文名：' + plainText(it, showPath), function(){
      copy(plainText(it, showPath), '已复制明文名');
    }));
    box.appendChild(chip('密文', '复制云盘上的名字：' + it.r, function(){
      copy(it.r, '已复制密文名');
    }));
    td4.appendChild(box);
    tr.appendChild(td1); tr.appendChild(td2); tr.appendChild(td3); tr.appendChild(td4);
    tr.onclick = function(){
      if (it.d) go(it.p); else copy(it.r, '已复制密文名');
    };
    return tr;
  }

  function render(){
    renderCrumbs();
    body.innerHTML = '';
    var list = byParent.get(cur) || [];
    var query = q.value.trim().toLowerCase();
    if (query) {
      list = items.filter(function(it){
        return it.p.toLowerCase().indexOf(query) >= 0 || (it.r && it.r.toLowerCase().indexOf(query) >= 0);
      });
    }
    list.forEach(function(it){ body.appendChild(row(it, !!query)); });
    empty.style.display = list.length ? 'none' : 'block';
    empty.textContent = query ? '没有匹配的条目。' : '这个目录下没有已索引的内容。';
  }

  q.addEventListener('input', render);
  q.addEventListener('keydown', function(e){
    if (e.key === 'Enter') {
      var v = q.value.trim().toLowerCase();
      if (!v) return;
      var hit = items.find(function(it){ return it.p.toLowerCase().indexOf(v) >= 0; });
      if (hit) { go(hit.d ? hit.p : parentOf(hit.p)); }
    }
  });

  document.getElementById('rescan').onclick = function(){
    var u = new URL(location.href);
    u.searchParams.set('rescan', '1');
    u.searchParams.set('t', Date.now());
    location.href = u.href;
  };

  if (new URL(location.href).searchParams.get('rescan') === '1') {
    showToast('已触发重建索引，稍后刷新本页查看进度');
  }

  var fromHash = decodeURIComponent(location.hash.slice(1) || '/');
  if (fromHash) cur = fromHash;
  render();
})();
</script>
</body>
</html>
`))
