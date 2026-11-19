package search

import (
	"context"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/search/meilisearch"
	"github.com/OpenListTeam/OpenList/v4/internal/search/searcher"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/mq"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	mapset "github.com/deckarep/golang-set/v2"
	log "github.com/sirupsen/logrus"
)

var (
	Quit = atomic.Pointer[chan struct{}]{}
)

func Running() bool {
	return Quit.Load() != nil
}

func BuildIndex(ctx context.Context, indexPaths, ignorePaths []string, maxDepth int, count bool) error {
	var (
		err      error
		objCount uint64 = 0
		fi       model.Obj
	)
	log.Infof("build index for: %+v", indexPaths)
	log.Infof("ignore paths: %+v", ignorePaths)
	quit := make(chan struct{}, 1)
	if !Quit.CompareAndSwap(nil, &quit) {
		// other goroutine is running
		return errs.BuildIndexIsRunning
	}
	var (
		indexMQ = mq.NewInMemoryMQ[ObjWithParent]()
		running = atomic.Bool{} // current goroutine running
		wg      = &sync.WaitGroup{}
	)
	running.Store(true)
	wg.Add(1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer func() {
			Quit.Store(nil)
			wg.Done()
			// notify walk to exit when StopIndex api called
			running.Store(false)
			ticker.Stop()
		}()
		tickCount := 0
		for {
			select {
			case <-ticker.C:
				tickCount += 1
				if indexMQ.Len() < 1000 && tickCount != 5 {
					continue
				} else if tickCount >= 5 {
					tickCount = 0
				}
				log.Infof("index obj count: %d", objCount)
				indexMQ.ConsumeAll(func(messages []mq.Message[ObjWithParent]) {
					if len(messages) != 0 {
						log.Debugf("current index: %s", messages[len(messages)-1].Content.Parent)
					}
					if err = BatchIndex(ctx, utils.MustSliceConvert(messages,
						func(src mq.Message[ObjWithParent]) ObjWithParent {
							return src.Content
						})); err != nil {
						log.Errorf("build index in batch error: %+v", err)
					} else {
						objCount = objCount + uint64(len(messages))
					}
					if count {
						WriteProgress(&model.IndexProgress{
							ObjCount:     objCount,
							IsDone:       false,
							LastDoneTime: nil,
						})
					}
				})

			case <-quit:
				log.Debugf("build index for %+v received quit", indexPaths)
				eMsg := ""
				now := time.Now()
				originErr := err
				indexMQ.ConsumeAll(func(messages []mq.Message[ObjWithParent]) {
					if err = BatchIndex(ctx, utils.MustSliceConvert(messages,
						func(src mq.Message[ObjWithParent]) ObjWithParent {
							return src.Content
						})); err != nil {
						log.Errorf("build index in batch error: %+v", err)
					} else {
						objCount = objCount + uint64(len(messages))
					}
					if originErr != nil {
						log.Errorf("build index error: %+v", originErr)
						eMsg = originErr.Error()
					} else {
						log.Infof("success build index, count: %d", objCount)
					}
					if count {
						// 构建失败时不能标记为 done，否则增量更新会被误放行。
						// 成功时才用本次完成时间，失败则沿用上一次成功时间。
						var lastDone *time.Time
						if eMsg == "" {
							lastDone = &now
						} else if p, pErr := Progress(); pErr == nil {
							lastDone = p.LastDoneTime
						}
						WriteProgress(&model.IndexProgress{
							ObjCount:     objCount,
							IsDone:       eMsg == "",
							LastDoneTime: lastDone,
							Error:        eMsg,
						})
					}
				})
				log.Debugf("build index for %+v quit success", indexPaths)
				return
			}
		}
	}()
	defer func() {
		if !running.Load() || Quit.Load() != &quit {
			log.Debugf("build index for %+v stopped by StopIndex", indexPaths)
			return
		}
		select {
		// avoid goroutine leak
		case quit <- struct{}{}:
		default:
		}
		wg.Wait()
	}()
	admin, err := op.GetAdmin()
	if err != nil {
		return err
	}
	if count {
		WriteProgress(&model.IndexProgress{
			ObjCount: 0,
			IsDone:   false,
		})
	}
	for _, indexPath := range indexPaths {
		walkFn := func(indexPath string, info model.Obj) error {
			if !running.Load() {
				return filepath.SkipDir
			}
			for _, avoidPath := range ignorePaths {
				if strings.HasPrefix(indexPath, avoidPath) {
					return filepath.SkipDir
				}
			}
			if storage, _, err := op.GetStorageAndActualPath(indexPath); err == nil {
				if storage.GetStorage().DisableIndex {
					return filepath.SkipDir
				}
			}
			// ignore root
			if indexPath == "/" {
				return nil
			}
			indexMQ.Publish(mq.Message[ObjWithParent]{
				Content: ObjWithParent{
					Obj:    info,
					Parent: path.Dir(indexPath),
				},
			})
			return nil
		}
		fi, err = fs.Get(ctx, indexPath, &fs.GetArgs{})
		if err != nil {
			return err
		}
		// TODO: run walkFS concurrently
		err = fs.WalkFS(context.WithValue(ctx, conf.UserKey, admin), maxDepth, indexPath, fi, walkFn)
		if err != nil {
			return err
		}
	}
	return nil
}

func Del(ctx context.Context, prefix string) error {
	return instance.Del(ctx, prefix)
}

func Clear(ctx context.Context) error {
	return instance.Clear(ctx)
}

func Config(ctx context.Context) searcher.Config {
	return instance.Config()
}

// isDisabledIndex 判断 parent 是否属于一个禁用了索引的存储。
// 全量构建会跳过 DisableIndex 的存储（见 walkFn），增量更新也必须保持一致，
// 否则这类存储在用户浏览目录时仍会被写进索引。
func isDisabledIndex(parent string) bool {
	storage, _, err := op.GetStorageAndActualPath(parent)
	if err != nil {
		return false
	}
	return storage.GetStorage().DisableIndex
}

func Update(ctx context.Context, parent string, objs []model.Obj) {
	if instance == nil || !instance.Config().AutoUpdate || !setting.GetBool(conf.AutoUpdateIndex) || Running() {
		return
	}
	if isIgnorePath(parent) || isDisabledIndex(parent) {
		return
	}
	// only update when index have built, and the build actually succeeded
	progress, err := Progress()
	if err != nil {
		log.Errorf("update search index error while get progress: %+v", err)
		return
	}
	if !progress.IsDone || progress.LastDoneTime == nil {
		return
	}
	doUpdate(ctx, parent, objs)
}

// UpdateForced 供索引 watcher 使用：按路径强制维护索引，绕过全局
// auto_update_index 开关与 index_progress 的完成状态判断。仍然尊重
// ignore_paths 与存储的 DisableIndex，并要求当前 searcher 支持增量更新。
// 全量构建期间本轮直接跳过（构建会重新遍历整棵树）。
func UpdateForced(ctx context.Context, parent string, objs []model.Obj) {
	if instance == nil || !instance.Config().AutoUpdate || Running() {
		return
	}
	if isIgnorePath(parent) || isDisabledIndex(parent) {
		return
	}
	doUpdate(ctx, parent, objs)
}

// doUpdate 执行真正的差异比对与索引写入。
func doUpdate(ctx context.Context, parent string, objs []model.Obj) {
	// Use task queue for Meilisearch to avoid race conditions with async indexing
	if msInstance, ok := instance.(interface {
		EnqueueUpdate(parent string)
	}); ok {
		// Only the parent path is queued; the object list is re-read from the
		// file system when the queue is consumed, so no stale snapshot is used.
		msInstance.EnqueueUpdate(parent)
		return
	}

	unlock := lockUpdate(parent)
	defer unlock()

	nodes, err := instance.Get(ctx, parent)
	if err != nil {
		log.Errorf("update search index error while get nodes: %+v", err)
		return
	}
	// 用「名字 + 目录标记 + 大小」做差异比对：同名文件被覆盖但大小变化时，
	// 旧实现只按名字比对会漏更新，导致搜索结果一直是旧数据。
	objKey := func(name string, isDir bool, size int64) string {
		return name + "\x00" + strconv.FormatBool(isDir) + "\x00" + strconv.FormatInt(size, 10)
	}
	now := mapset.NewSet[string]()
	for i := range objs {
		now.Add(objKey(objs[i].GetName(), objs[i].IsDir(), objs[i].GetSize()))
	}
	old := mapset.NewSet[string]()
	for i := range nodes {
		old.Add(objKey(nodes[i].Name, nodes[i].IsDir, nodes[i].Size))
	}
	// delete data that no longer exists or has changed
	toDelete := old.Difference(now)
	toAdd := now.Difference(old)
	deleted := make(map[string]bool)
	for i := range nodes {
		key := objKey(nodes[i].Name, nodes[i].IsDir, nodes[i].Size)
		if toDelete.Contains(key) && !deleted[nodes[i].Name] &&
			!op.HasStorage(path.Join(parent, nodes[i].Name)) {
			deleted[nodes[i].Name] = true
			log.Debugf("delete index: %s", path.Join(parent, nodes[i].Name))
			err = instance.Del(ctx, path.Join(parent, nodes[i].Name))
			if err != nil {
				log.Errorf("update search index error while del old node: %+v", err)
				return
			}
		}
	}
	// collect files and folders to add in batch
	added := make(map[string]bool)
	var toAddObjs []ObjWithParent
	for i := range objs {
		key := objKey(objs[i].GetName(), objs[i].IsDir(), objs[i].GetSize())
		if toAdd.Contains(key) && !added[objs[i].GetName()] {
			added[objs[i].GetName()] = true
			log.Debugf("add index: %s", path.Join(parent, objs[i].GetName()))
			toAddObjs = append(toAddObjs, ObjWithParent{
				Parent: parent,
				Obj:    objs[i],
			})
		}
	}
	// batch index all files and folders at once
	if len(toAddObjs) > 0 {
		err = BatchIndex(ctx, toAddObjs)
		if err != nil {
			log.Errorf("update search index error while batch index new nodes: %+v", err)
			return
		}
	}
}

// listCurrentObjects lists the live children of a mount path using an admin
// context, matching the behaviour of BuildIndex. It is injected into the
// Meilisearch task queue so updates are diffed against the current state
// rather than the snapshot captured when the hook fired.
func listCurrentObjects(ctx context.Context, parent string) ([]model.Obj, error) {
	admin, err := op.GetAdmin()
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, conf.UserKey, admin)
	return fs.List(ctx, parent, &fs.ListArgs{
		NoLog:    true,
		SkipHook: true,
	})
}

func init() {
	// Provide the Meilisearch task queue with a way to re-list directories at
	// consumption time.
	meilisearch.SetListObjectsFunc(listCurrentObjects)
	op.RegisterObjsUpdateHook(Update)
}
