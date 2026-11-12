package crypt

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	stdpath "path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/namemap"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	log "github.com/sirupsen/logrus"
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

type nameMapRow struct {
	Indent    int
	Name      string
	Raw       string
	RawPath   string
	Dir       bool
	Size      string
	PlainPath string
}

type nameMapPage struct {
	Title      string
	Mount      string
	StorageID  uint
	Updated    string
	TotalDirs  int
	TotalFiles int
	Rows       []nameMapRow
}

// 每个存储实例各自持有一份，避免跨驱动共享。
type nameMapCache struct {
	mu     sync.Mutex
	loaded bool
	items  map[string]nameMapEntry
	timer  *time.Timer
}

var nameMapCaches sync.Map // storageID -> *nameMapCache

func (d *Crypt) nameMapCache() *nameMapCache {
	if v, ok := nameMapCaches.Load(d.ID); ok {
		return v.(*nameMapCache)
	}
	v, _ := nameMapCaches.LoadOrStore(d.ID, &nameMapCache{items: map[string]nameMapEntry{}})
	return v.(*nameMapCache)
}

// recordNameMap 在每次列目录时记录「显示名 → 底层名」的对应关系。
// 写入做了防抖：短时间内多次列目录只落盘一次。
func (d *Crypt) recordNameMap(reqPath string, objs []model.Obj) {
	if reqPath == "" || len(objs) == 0 {
		return
	}
	c := d.nameMapCache()
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.loaded {
		c.load(d.ID)
	}

	changed := false
	for _, o := range objs {
		rawPath := o.GetPath()
		if rawPath == "" {
			continue
		}
		name := o.GetName()
		e := nameMapEntry{
			Plain:     name,
			Raw:       stdpath.Base(rawPath),
			PlainPath: stdpath.Join(reqPath, name),
			RawPath:   rawPath,
			Dir:       o.IsDir(),
			Size:      o.GetSize(),
		}
		if old, ok := c.items[e.RawPath]; !ok || old != e {
			c.items[e.RawPath] = e
			changed = true
		}
	}
	if !changed || c.timer != nil {
		return
	}
	c.timer = time.AfterFunc(2*time.Second, d.flushNameMap)
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

func renderNameMap(id uint, mount string, items []nameMapEntry) string {
	sort.Slice(items, func(i, j int) bool {
		if items[i].PlainPath == items[j].PlainPath {
			return items[i].RawPath < items[j].RawPath
		}
		return items[i].PlainPath < items[j].PlainPath
	})
	rows := make([]nameMapRow, 0, len(items))
	dirs, files := 0, 0
	for _, e := range items {
		depth := strings.Count(strings.Trim(e.PlainPath, "/"), "/")
		rows = append(rows, nameMapRow{
			Indent:    depth * 16,
			Name:      stdpath.Base(e.PlainPath),
			Raw:       e.Raw,
			RawPath:   e.RawPath,
			Dir:       e.Dir,
			Size:      humanSize(e.Size),
			PlainPath: e.PlainPath,
		})
		if e.Dir {
			dirs++
		} else {
			files++
		}
	}
	page := nameMapPage{
		Title:      "明文 ↔ 密文对照",
		Mount:      mount,
		StorageID:  id,
		Updated:    time.Now().Format("2006-01-02 15:04:05"),
		TotalDirs:  dirs,
		TotalFiles: files,
		Rows:       rows,
	}
	var sb strings.Builder
	if err := nameMapTemplate.Execute(&sb, page); err != nil {
		return "<!doctype html><meta charset=\"utf-8\"><p>render failed: " + err.Error()
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

var nameMapTemplate = template.Must(template.New("namemap").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · {{.Mount}}</title>
<style>
:root{color-scheme:light dark}
*{box-sizing:border-box}
body{margin:0;padding:24px;font:14px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;background:#faf9f6;color:#26262a}
header{max-width:1100px;margin:0 auto 16px}
h1{margin:0 0 4px;font-size:18px;font-weight:600}
.meta{color:#6b6b72;font-size:12px}
.tools{max-width:1100px;margin:0 auto 12px;display:flex;gap:12px;align-items:center}
input{flex:1;padding:8px 12px;border:1px solid rgba(0,0,0,.18);border-radius:8px;background:transparent;color:inherit;font:inherit;outline:none}
input:focus{border-color:#534ab7}
table{max-width:1100px;margin:0 auto;width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:6px 10px;border-bottom:1px solid rgba(0,0,0,.08);vertical-align:top}
th{position:sticky;top:0;background:#faf9f6;font-weight:600;font-size:12px;color:#6b6b72}
td.name{white-space:nowrap}
td.raw{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px;color:#6b6b72;word-break:break-all}
.tag{display:inline-block;min-width:30px;font-size:11px;color:#6b6b72}
.count{max-width:1100px;margin:12px auto 0;color:#6b6b72;font-size:12px}
@media (prefers-color-scheme: dark){
body{background:#1c1c1e;color:#ececee}
th{background:#1c1c1e}
.meta,.count,.tag,td.raw{color:#a0a0a8}
th,td{border-color:rgba(255,255,255,.12)}
input{border-color:rgba(255,255,255,.22)}
input:focus{border-color:#a9a4f0}
}
</style>
</head>
<body>
<header>
<h1>{{.Title}}</h1>
<div class="meta">挂载点 {{.Mount}} · 存储 ID {{.StorageID}} · 更新于 {{.Updated}}</div>
</header>
<div class="tools">
<input id="q" type="search" placeholder="搜索明文名或密文名…" autocomplete="off">
</div>
<table id="t">
<thead><tr><th style="width:56px">类型</th><th>明文路径</th><th>云盘上的名字</th></tr></thead>
<tbody>
{{range .Rows}}<tr data-q="{{.PlainPath}} {{.Raw}}">
<td class="tag">{{if .Dir}}目录{{else}}文件{{end}}</td>
<td class="name"><span style="display:inline-block;width:{{.Indent}}px"></span>{{.Name}}</td>
<td class="raw">{{.Raw}}{{if not .Dir}} <span class="tag">{{.Size}}</span>{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
<div class="count">共 {{.TotalDirs}} 个目录、{{.TotalFiles}} 个文件。列表刷新后自动更新。</div>
<script>
const q=document.getElementById('q'),rows=[...document.querySelectorAll('#t tbody tr')];
q.addEventListener('input',()=>{const v=q.value.trim().toLowerCase();for(const r of rows){r.style.display=!v||r.dataset.q.toLowerCase().includes(v)?'':'none'}});
</script>
</body>
</html>
`))

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
