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
			details, e := op.GetStorageDetails(ctx, dri)
			if e != nil {
				if !errors.Is(e, errs.NotImplement) && !errors.Is(e, errs.StorageNotInit) {
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
	paths := strings.ReplaceAll(addition.Paths, "\r\n", "\n")
	for _, line := range strings.Split(paths, "\n") {
		if strings.TrimSpace(line) == newLine {
			// already collected, idempotent success
			common.SuccessResp(c)
			return
		}
	}

	if strings.TrimSpace(paths) == "" {
		addition.Paths = newLine
	} else {
		addition.Paths = strings.TrimRight(paths, "\n") + "\n" + newLine
	}

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
	paths := strings.ReplaceAll(addition.Paths, "\r\n", "\n")
	var entries []favoriteEntry
	for _, line := range strings.Split(paths, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
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

	paths := strings.ReplaceAll(addition.Paths, "\r\n", "\n")
	var kept []string
	for _, line := range strings.Split(paths, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
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
	addition.Paths = strings.Join(kept, "\n")

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
