package handles

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/alias"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type StorageResp struct {
	model.Storage
	MountDetails *model.StorageDetails `json:"mount_details,omitempty"`
}

type detailWithIndex struct {
	idx int
	val *model.StorageDetails
}

func makeStorageResp(ctx *gin.Context, storages []model.Storage) []*StorageResp {
	waitCtx, cancel := context.WithTimeout(ctx.Request.Context(), time.Second*3)
	defer cancel()
	ret := make([]*StorageResp, len(storages))
	detailsChan := make(chan detailWithIndex, len(storages))
	workerCount := 0
	for i, s := range storages {
		ret[i] = &StorageResp{
			Storage:      s,
			MountDetails: nil,
		}
		if setting.GetBool(conf.HideStorageDetailsInManagePage) {
			continue
		}
		d, err := op.GetStorageByMountPath(s.MountPath)
		if err != nil {
			continue
		}
		_, ok := d.(driver.WithDetails)
		if !ok {
			continue
		}
		workerCount++
		go func(dri driver.Driver, idx int) {
			details, e := op.GetStorageDetails(waitCtx, dri)
			if e != nil {
				if !errors.Is(e, errs.NotImplement) && !errors.Is(e, errs.StorageNotInit) &&
					!errors.Is(e, context.DeadlineExceeded) && !errors.Is(e, context.Canceled) {
					log.Errorf("failed get %s details: %+v", dri.GetStorage().MountPath, e)
				}
			}
			detailsChan <- detailWithIndex{idx: idx, val: details}
		}(d, i)
	}
	for workerCount > 0 {
		select {
		case r := <-detailsChan:
			ret[r.idx].MountDetails = r.val
			workerCount--
		case <-time.After(time.Second * 3):
			workerCount = 0
		}
	}
	return ret
}

func ListStorages(c *gin.Context) {
	var req model.PageReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	log.Debugf("%+v", req)
	storages, total, err := db.GetStorages(req.Page, req.PerPage)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, common.PageResp{
		Content: makeStorageResp(c, storages),
		Total:   total,
	})
}

func CreateStorage(c *gin.Context) {
	var req model.Storage
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if id, err := op.CreateStorage(c.Request.Context(), req); err != nil {
		common.ErrorWithDataResp(c, err, 500, gin.H{
			"id": id,
		}, true)
	} else {
		common.SuccessResp(c, gin.H{
			"id": id,
		})
	}
}

func UpdateStorage(c *gin.Context) {
	var req model.Storage
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.UpdateStorage(c.Request.Context(), req); err != nil {
		common.ErrorResp(c, err, 500, true)
	} else {
		common.SuccessResp(c)
	}
}

func DeleteStorage(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.DeleteStorageById(c.Request.Context(), uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

func DisableStorage(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.DisableStorage(c.Request.Context(), uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

func EnableStorage(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if err := op.EnableStorage(c.Request.Context(), uint(id)); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

func GetStorage(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	storage, err := db.GetStorageById(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, storage)
}

// collectMutex guards the read-modify-write of an Alias storage's paths when
// collecting favorites, so concurrent requests don't drop lines.
var collectMutex sync.Mutex

// favoriteRootPlaceholder 是收藏别名存储里的占位根条目。
//
// Alias 驱动在 paths 只有「一个不同显示名」时（Init 的 case 1），会把这条
// 唯一记录当成整个挂载的根目录。收藏场景下这会导致「只收藏了一个文件」时，
// /收藏 根目录指向该文件本身，列表页因此显示为空。
//
// 为解决该问题，收藏功能始终在 paths 中保留一条占位目录项，
// 使不同显示名数量 >= 2，从而让 Alias 走 default 分支，把 /收藏 呈现为
// 一个正常的只读目录，真实收藏项作为其子项展示。
//
// 占位项必须指向一个不存在的路径，而不是 "/"：Alias 的 listRoot 会把每个
// 不同显示名都作为根目录下的条目列出，若占位项指向 "/"，会把整个根存储的
// 内容（所有其它挂载点）都列进 /收藏。指向不存在的路径时，listRoot 探测失败，
// 只会生成一个空的占位文件夹对象。
//
// 占位项在 ListCollects 中会被过滤，前端也会按名字隐藏，不会暴露给用户。
const (
	favoriteRootPlaceholderName = "__favorite_root__"
	favoriteRootPlaceholderLine = favoriteRootPlaceholderName + ":/.openlist_favorite_placeholder"
)

// isFavoritePlaceholderLine 判断某一行是否为占位根条目。
func isFavoritePlaceholderLine(line string) bool {
	return strings.TrimSpace(line) == favoriteRootPlaceholderLine
}

// splitFavoriteLines 把 paths 拆成非空行，统一换行符并去掉首尾空白。
func splitFavoriteLines(paths string) []string {
	paths = strings.ReplaceAll(paths, "\r\n", "\n")
	var lines []string
	for _, line := range strings.Split(paths, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// ensureFavoritePlaceholder 保证给定的 paths 中始终存在占位根条目，
// 并返回整理后的行列表（占位项固定在首位）。
func ensureFavoritePlaceholder(lines []string) []string {
	kept := make([]string, 0, len(lines)+1)
	kept = append(kept, favoriteRootPlaceholderLine)
	for _, line := range lines {
		if isFavoritePlaceholderLine(line) {
			continue
		}
		kept = append(kept, line)
	}
	return kept
}

// CollectToFavorite appends a source path to the paths config of the Alias
// storage mounted at req.MountPath (default "/收藏"), so the item is aggregated
// under that mount point.
func CollectToFavorite(c *gin.Context) {
	var req struct {
		Path      string `json:"path" binding:"required"`
		Name      string `json:"name"`
		MountPath string `json:"mount_path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if req.Path == "" || !strings.HasPrefix(req.Path, "/") {
		common.ErrorResp(c, errors.New("path is required and must start with '/'"), 400)
		return
	}
	// 数据库中 mount_path 以 "/" 开头（如 "/收藏"），这里构造规范的挂载路径，
	// 不能去掉前导 "/"，否则永远匹配不到实际存储的 mount_path。
	mount := "/" + strings.Trim(req.MountPath, "/")
	if mount == "/" {
		mount = "/收藏"
	}

	// 禁止收藏收藏目录自身内部的路径，否则会形成指向自己的循环引用，
	// 导致该条目被当成目录、显示异常。
	if req.Path == mount || strings.HasPrefix(req.Path, mount+"/") {
		common.ErrorResp(c, errors.Errorf("cannot collect a path inside the favorite mount %s", mount), 400)
		return
	}

	// 显示名优先用传入的 name，否则取路径最后一段，保证不同收藏项可区分。
	// 避免所有收藏项都叫同一个名字而被 Alias 当作负载均衡副本。
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = pathBase(req.Path)
	}

	collectMutex.Lock()
	defer collectMutex.Unlock()

	// 兼容数据库中 "/收藏" 与 "/收藏/" 两种写法。
	target, err := db.GetStorageByMountPath(mount)
	if err != nil {
		target, err = db.GetStorageByMountPath(mount + "/")
	}
	if err != nil {
		common.ErrorResp(c, errors.Errorf("未找到 %s 别名存储，请先在后台添加 Alias 存储并挂载到 %s", mount, mount), 404)
		return
	}
	if target.Driver != "Alias" {
		common.ErrorResp(c, errors.New("target storage is not an Alias driver"), 400)
		return
	}

	var addition alias.Addition
	if target.Addition != "" {
		if err := json.Unmarshal([]byte(target.Addition), &addition); err != nil {
			common.ErrorResp(c, errors.WithMessage(err, "failed to parse alias addition"), 500)
			return
		}
	}

	newLine := name + ":" + req.Path
	lines := splitFavoriteLines(addition.Paths)
	for _, line := range lines {
		if line == newLine {
			// already collected, idempotent success
			common.SuccessResp(c)
			return
		}
	}

	lines = append(lines, newLine)
	// 始终保留占位根，避免只收藏一条时被 Alias 当成根目录。
	addition.Paths = strings.Join(ensureFavoritePlaceholder(lines), "\n")

	data, err := json.Marshal(addition)
	if err != nil {
		common.ErrorResp(c, errors.WithMessage(err, "failed to marshal alias addition"), 500)
		return
	}
	target.Addition = string(data)

	if err := op.UpdateStorage(c.Request.Context(), *target); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

// findFavoriteStorage 定位收藏用的 Alias 存储。
func findFavoriteStorage(mountPath string) (*model.Storage, string, error) {
	mount := "/" + strings.Trim(mountPath, "/")
	if mount == "/" {
		mount = "/收藏"
	}
	target, err := db.GetStorageByMountPath(mount)
	if err != nil {
		target, err = db.GetStorageByMountPath(mount + "/")
	}
	if err != nil {
		return nil, mount, errors.Errorf("未找到 %s 别名存储，请先在后台添加 Alias 存储并挂载到 %s", mount, mount)
	}
	if target.Driver != "Alias" {
		return nil, mount, errors.New("target storage is not an Alias driver")
	}
	return target, mount, nil
}

// parseFavoritePaths 解析别名存储的 paths，返回「显示名 -> 源路径」映射与原始行。
type favoriteEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Line string `json:"line"`
}

func parseFavoritePaths(additionStr string) []favoriteEntry {
	var addition alias.Addition
	if additionStr != "" {
		_ = json.Unmarshal([]byte(additionStr), &addition)
	}
	var entries []favoriteEntry
	for _, line := range splitFavoriteLines(addition.Paths) {
		if isFavoritePlaceholderLine(line) {
			// 占位根不属于真实收藏项，不暴露给前端。
			continue
		}
		name, path, ok := strings.Cut(line, ":")
		if !ok || strings.Contains(name, "/") {
			// 无显示名的行，按路径最后一段命名
			name = pathBase(line)
			path = line
		}
		entries = append(entries, favoriteEntry{Name: name, Path: path, Line: line})
	}
	return entries
}

// ListCollects 返回收藏别名存储中已收藏的条目列表。
func ListCollects(c *gin.Context) {
	mountPath := c.Query("mount_path")
	target, _, err := findFavoriteStorage(mountPath)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	common.SuccessResp(c, parseFavoritePaths(target.Addition))
}

// RemoveFavorite 从收藏别名存储的 paths 中删除指定源路径。
func RemoveFavorite(c *gin.Context) {
	var req struct {
		Path      string `json:"path" binding:"required"`
		MountPath string `json:"mount_path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if req.Path == "" || !strings.HasPrefix(req.Path, "/") {
		common.ErrorResp(c, errors.New("path is required and must start with '/'"), 400)
		return
	}

	collectMutex.Lock()
	defer collectMutex.Unlock()

	target, _, err := findFavoriteStorage(req.MountPath)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}

	var addition alias.Addition
	if target.Addition != "" {
		if err := json.Unmarshal([]byte(target.Addition), &addition); err != nil {
			common.ErrorResp(c, errors.WithMessage(err, "failed to parse alias addition"), 500)
			return
		}
	}

	var kept []string
	for _, trimmed := range splitFavoriteLines(addition.Paths) {
		if isFavoritePlaceholderLine(trimmed) {
			// 占位根始终保留，最后统一补回。
			continue
		}
		_, p, ok := strings.Cut(trimmed, ":")
		if !ok {
			p = trimmed
		}
		if p == req.Path {
			// 命中要删除的收藏项，跳过
			continue
		}
		kept = append(kept, trimmed)
	}
	// 即使收藏清空也保留占位根，保持 /收藏 目录结构稳定。
	addition.Paths = strings.Join(ensureFavoritePlaceholder(kept), "\n")

	data, err := json.Marshal(addition)
	if err != nil {
		common.ErrorResp(c, errors.WithMessage(err, "failed to marshal alias addition"), 500)
		return
	}
	target.Addition = string(data)

	if err := op.UpdateStorage(c.Request.Context(), *target); err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c)
}

// pathBase 返回路径最后一段作为显示名。
func pathBase(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return p
	}
	return p[idx+1:]
}

func LoadAllStorages(c *gin.Context) {
	storages, err := db.GetEnabledStorages()
	if err != nil {
		log.Errorf("failed get enabled storages: %+v", err)
		common.ErrorResp(c, err, 500, true)
		return
	}
	conf.ResetStoragesLoadSignal()
	go func(storages []model.Storage) {
		for _, storage := range storages {
			storageDriver, err := op.GetStorageByMountPath(storage.MountPath)
			if err != nil {
				log.Errorf("failed get storage driver: %+v", err)
				continue
			}
			// drop the storage in the driver
			if err := storageDriver.Drop(context.Background()); err != nil {
				log.Errorf("failed drop storage: %+v", err)
				continue
			}
			if err := op.LoadStorage(context.Background(), storage); err != nil {
				log.Errorf("failed get enabled storages: %+v", err)
				continue
			}
			log.Infof("success load storage: [%s], driver: [%s]",
				storage.MountPath, storage.Driver)
		}
		conf.SendStoragesLoadedSignal()
	}(storages)
	common.SuccessResp(c)
}
