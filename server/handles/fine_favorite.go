package handles

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"

	"github.com/OpenListTeam/OpenList/v4/drivers/alias"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	log "github.com/sirupsen/logrus"
)

// 二级收藏夹同步：在 /收藏 这个 Alias 驱动的配置框里填写名字后，
// 后端自动维护 /收藏/<名字> 的子 Alias 存储，作为更细化的收藏夹。
const (
	fineFavFavoriteMount = "/收藏"
	// 用 Remark 前缀标记「由本功能自动创建」的子存储，避免误删/覆盖用户自己挂的同名存储。
	fineFavRemarkPrefix = "__ff_child__:"
	// 子存储的初始 paths：与默认收藏夹一致，用占位根保证空收藏夹也能正常列出。
	fineFavMaxItems     = 100
	fineFavMaxNameRunes = 64
)

var fineFavMutex sync.Mutex

// normalizeFineFavMount 把挂载路径规范成 /xxx 形式（去首尾斜杠、折叠重复斜杠）。
func normalizeFineFavMount(p string) string {
	p = "/" + strings.Trim(p, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

// parseFineFavoriteNames 解析配置框文本：每行一个名字，去掉非法/重复项。
func parseFineFavoriteNames(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	seen := make(map[string]bool)
	var names []string
	for _, line := range strings.Split(raw, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			continue
		}
		runes := []rune(name)
		if len(runes) > fineFavMaxNameRunes {
			name = string(runes[:fineFavMaxNameRunes])
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
		if len(names) >= fineFavMaxItems {
			break
		}
	}
	return names
}

// syncFineFavoriteStorages 在保存 /收藏 别名存储后调用：按配置框内容
// 创建、排序、删除对应的 /收藏/<名字> 子 Alias 存储。
// 只有挂载点恰好是 /收藏 的 Alias 才会触发；子存储的创建/更新不会再次触发本函数
// （本函数只在 HTTP handler 层调用，op 层不会回调）。
func syncFineFavoriteStorages(ctx context.Context, parent model.Storage) {
	if parent.Driver != "Alias" || normalizeFineFavMount(parent.MountPath) != fineFavFavoriteMount {
		return
	}

	fineFavMutex.Lock()
	defer fineFavMutex.Unlock()

	var addition alias.Addition
	if parent.Addition != "" {
		if err := json.Unmarshal([]byte(parent.Addition), &addition); err != nil {
			log.Warnf("[fine-favorite] failed to parse /收藏 addition: %v", err)
			return
		}
	}
	names := parseFineFavoriteNames(addition.FineFavorites)

	// 期望状态：挂载点 -> order（按文本顺序，紧跟父存储之后）。
	desired := make(map[string]int, len(names))
	baseOrder := parent.Order * 100
	for i, name := range names {
		mount := fineFavFavoriteMount + "/" + name
		desired[normalizeFineFavMount(mount)] = baseOrder + i + 1
	}

	all, _, err := db.GetStorages(1, math.MaxInt)
	if err != nil {
		log.Errorf("[fine-favorite] failed to list storages: %v", err)
		return
	}

	// 现存由本功能创建的子存储。
	existing := make(map[string]model.Storage)
	for _, s := range all {
		if strings.HasPrefix(s.Remark, fineFavRemarkPrefix) {
			existing[normalizeFineFavMount(s.MountPath)] = s
		}
	}

	// 创建或调整顺序。
	for mount, order := range desired {
		if s, ok := existing[mount]; ok {
			if s.Order != order {
				s.Order = order
				if err := op.UpdateStorage(ctx, s); err != nil {
					log.Errorf("[fine-favorite] failed to reorder %s: %v", mount, err)
				}
			}
			continue
		}
		// 挂载点已被别的存储占用：不覆盖，避免破坏用户数据。
		if _, err := db.GetStorageByMountPath(mount); err == nil {
			log.Warnf("[fine-favorite] mount %s already exists and is not managed, skip", mount)
			continue
		}
		childAddition := alias.Addition{
			Paths:               favoriteRootPlaceholderLine,
			ReadConflictPolicy:  "first",
			WriteConflictPolicy: "disabled",
			PutConflictPolicy:   "disabled",
		}
		data, err := json.Marshal(childAddition)
		if err != nil {
			log.Errorf("[fine-favorite] failed to marshal addition for %s: %v", mount, err)
			continue
		}
		child := model.Storage{
			MountPath: mount,
			Order:     order,
			Driver:    "Alias",
			Addition:  string(data),
			Remark:    fineFavRemarkPrefix + "auto",
		}
		if _, err := op.CreateStorage(ctx, child); err != nil {
			log.Errorf("[fine-favorite] failed to create %s: %v", mount, err)
		}
	}

	// 删除文本里已经去掉的子存储（仅限本功能创建的）。
	for mount, s := range existing {
		if _, ok := desired[mount]; ok {
			continue
		}
		if err := op.DeleteStorageById(ctx, s.ID); err != nil {
			log.Errorf("[fine-favorite] failed to delete %s: %v", mount, err)
		}
	}
}

// removeAllFineFavoriteStorages 在删除 /收藏 存储时清理其自动创建的子存储。
func removeAllFineFavoriteStorages(ctx context.Context) {
	fineFavMutex.Lock()
	defer fineFavMutex.Unlock()

	all, _, err := db.GetStorages(1, math.MaxInt)
	if err != nil {
		log.Errorf("[fine-favorite] failed to list storages: %v", err)
		return
	}
	for _, s := range all {
		if strings.HasPrefix(s.Remark, fineFavRemarkPrefix) {
			if err := op.DeleteStorageById(ctx, s.ID); err != nil {
				log.Errorf("[fine-favorite] failed to delete %s: %v", s.MountPath, err)
			}
		}
	}
}
