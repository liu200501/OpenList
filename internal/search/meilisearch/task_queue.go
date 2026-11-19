package meilisearch

import (
	"context"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	mapset "github.com/deckarep/golang-set/v2"
	log "github.com/sirupsen/logrus"
)

// listObjectsFunc lists the current children of a parent path from the file
// system. It is injected by the search package (which is allowed to import
// internal/fs) so that this package does not have to import fs itself and
// risk an import cycle: internal/fs -> internal/op -> ... -> internal/search.
type listObjectsFunc func(ctx context.Context, parent string) ([]model.Obj, error)

// QueuedTask represents a pending index update. Only the parent path is kept:
// the object list is re-read from the file system when the task is consumed,
// so a queued update can never act on a stale snapshot.
type QueuedTask struct {
	Parent    string
	Depth     int       // path depth for sorting (shallow parents first)
	EnqueueAt time.Time // enqueue time
}

// TaskQueueManager manages the task queue for async index operations.
type TaskQueueManager struct {
	queue        map[string]*QueuedTask // parent -> task
	pendingTasks map[string][]int64     // parent -> submitted taskUIDs not yet known to be finished
	mu           sync.RWMutex
	ticker       *time.Ticker
	stopOnce     sync.Once
	stopCh       chan struct{}
	m            *Meilisearch
	consuming    atomic.Bool // prevents concurrent consumption
	listObjects  listObjectsFunc
}

// statusCheckTimeout bounds how long a single pending-task status read may
// take, so a slow/unreachable Meilisearch server can never wedge the consumer.
const (
	consumeInterval    = 30 * time.Second
	statusCheckTimeout = 10 * time.Second
)

// NewTaskQueueManager creates a new task queue manager.
func NewTaskQueueManager(m *Meilisearch, listObjects listObjectsFunc) *TaskQueueManager {
	return &TaskQueueManager{
		queue:        make(map[string]*QueuedTask),
		pendingTasks: make(map[string][]int64),
		stopCh:       make(chan struct{}),
		m:            m,
		listObjects:  listObjects,
	}
}

// calculateDepth calculates the depth of a path.
func calculateDepth(p string) int {
	if p == "/" {
		return 0
	}
	return strings.Count(strings.Trim(p, "/"), "/") + 1
}

// Enqueue queues an update for a parent. Repeated updates for the same parent
// are deduplicated; because only the path is stored, deduplication can never
// discard a newer file-system snapshot.
func (tqm *TaskQueueManager) Enqueue(parent string) {
	tqm.mu.Lock()
	defer tqm.mu.Unlock()

	if _, exists := tqm.queue[parent]; exists {
		return
	}
	tqm.queue[parent] = &QueuedTask{
		Parent:    parent,
		Depth:     calculateDepth(parent),
		EnqueueAt: time.Now(),
	}
	log.Debugf("enqueued update task for parent: %s, depth: %d", parent, calculateDepth(parent))
}

// Start starts the task queue consumer.
func (tqm *TaskQueueManager) Start() {
	tqm.ticker = time.NewTicker(consumeInterval)
	go func() {
		for {
			select {
			case <-tqm.ticker.C:
				tqm.consume()
			case <-tqm.stopCh:
				log.Info("task queue manager stopped")
				return
			}
		}
	}()
	log.Infof("task queue manager started, will consume every %s", consumeInterval)
}

// Stop stops the task queue consumer. It is safe to call more than once.
func (tqm *TaskQueueManager) Stop() {
	tqm.stopOnce.Do(func() {
		if tqm.ticker != nil {
			tqm.ticker.Stop()
		}
		close(tqm.stopCh)
	})
}

// consume processes all tasks in the queue.
func (tqm *TaskQueueManager) consume() {
	if !tqm.consuming.CompareAndSwap(false, true) {
		log.Warn("previous consume still running, skip this round")
		return
	}
	defer tqm.consuming.Store(false)

	tqm.mu.Lock()
	tasks := make([]*QueuedTask, 0, len(tqm.queue))
	for _, task := range tqm.queue {
		tasks = append(tasks, task)
	}
	// clear queue; skipped tasks are re-enqueued below
	tqm.queue = make(map[string]*QueuedTask)
	tqm.mu.Unlock()

	if len(tasks) == 0 {
		return
	}
	log.Infof("consuming task queue: %d tasks", len(tasks))

	// Shallow parents first so a directory is indexed before its children.
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Depth != tasks[j].Depth {
			return tasks[i].Depth < tasks[j].Depth
		}
		return tasks[i].EnqueueAt.Before(tasks[j].EnqueueAt)
	})

	ctx := context.Background()

	for _, task := range tasks {
		parent := task.Parent

		// Wait for the parent's previous Meilisearch tasks to settle before
		// diffing again, otherwise concurrent tasks race with each other.
		tqm.mu.RLock()
		prevUIDs := append([]int64(nil), tqm.pendingTasks[parent]...)
		tqm.mu.RUnlock()
		if len(prevUIDs) > 0 {
			stillRunning := false
			for _, uid := range prevUIDs {
				done, err := tqm.taskDone(ctx, uid)
				if err != nil {
					log.Warnf("failed to check task %d status for parent %s: %v", uid, parent, err)
					// On error, be conservative and retry this parent next round
					// instead of running a diff against a possibly stale index.
					stillRunning = true
					break
				}
				if !done {
					stillRunning = true
					break
				}
			}
			if stillRunning {
				log.Debugf("parent %s has unfinished tasks, re-enqueueing for next round", parent)
				tqm.reEnqueue(task)
				continue
			}
			tqm.mu.Lock()
			delete(tqm.pendingTasks, parent)
			tqm.mu.Unlock()
		}

		tqm.executeTask(ctx, parent)
	}
}

// taskDone reports whether the given task reached a terminal state. It uses a
// non-blocking status read with a bounded context, so a slow/stuck task can
// never wedge the consumer (WaitForTaskWithContext would block until terminal).
func (tqm *TaskQueueManager) taskDone(ctx context.Context, taskUID int64) (bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, statusCheckTimeout)
	defer cancel()
	task, err := tqm.m.Client.GetTaskWithContext(waitCtx, taskUID)
	if err != nil {
		return false, err
	}
	// Only enqueued/processing are non-terminal. Everything else (succeeded,
	// failed, canceled) is terminal. Comparing the underlying string keeps
	// this robust against constant renames across meilisearch-go versions.
	status := string(task.Status)
	if status == "enqueued" || status == "processing" {
		return false, nil
	}
	if status != "succeeded" {
		log.Warnf("meilisearch task %d ended with status %s", taskUID, status)
	}
	return true, nil
}

// reEnqueue puts a skipped task back without overwriting a newer entry.
func (tqm *TaskQueueManager) reEnqueue(task *QueuedTask) {
	tqm.mu.Lock()
	defer tqm.mu.Unlock()
	if _, exists := tqm.queue[task.Parent]; !exists {
		tqm.queue[task.Parent] = task
	}
}

// executeTask diffs the current index against the live file system and
// applies the changes.
func (tqm *TaskQueueManager) executeTask(ctx context.Context, parent string) {
	// Re-list the parent at consumption time so we never work from a stale
	// snapshot captured when the hook fired.
	currentObjs, err := tqm.listObjects(ctx, parent)
	if err != nil {
		log.Errorf("failed to list current objects for parent %s: %v", parent, err)
		return
	}

	nodes, err := tqm.m.Get(ctx, parent)
	if err != nil {
		log.Errorf("failed to get indexed nodes for parent %s: %v", parent, err)
		return
	}

	now := mapset.NewSet[string]()
	for i := range currentObjs {
		now.Add(currentObjs[i].GetName())
	}
	old := mapset.NewSet[string]()
	for i := range nodes {
		old.Add(nodes[i].Name)
	}

	toDelete := old.Difference(now)
	toAdd := now.Difference(old)

	// Collect paths to delete. HasStorage excludes mounted storages, which are
	// not ordinary child objects and must not be removed from the index here.
	var pathsToDelete []string
	for i := range nodes {
		if toDelete.Contains(nodes[i].Name) && !op.HasStorage(path.Join(parent, nodes[i].Name)) {
			pathsToDelete = append(pathsToDelete, path.Join(parent, nodes[i].Name))
		}
	}

	var allTaskUIDs []int64

	if len(pathsToDelete) > 0 {
		log.Debugf("executing delete for parent %s: %d paths", parent, len(pathsToDelete))
		uids, err := tqm.m.batchDeleteWithTaskUID(ctx, pathsToDelete)
		if err != nil {
			log.Errorf("failed to batch delete for parent %s: %v", parent, err)
		} else {
			allTaskUIDs = append(allTaskUIDs, uids...)
		}
	}

	// Collect the nodes to add. New directories are expanded recursively so
	// their children become searchable without a manual full rebuild.
	var nodesToAdd []model.SearchNode
	for i := range currentObjs {
		obj := currentObjs[i]
		if !toAdd.Contains(obj.GetName()) {
			continue
		}
		nodePath := path.Join(parent, obj.GetName())
		if obj.IsDir() {
			uids := tqm.indexNewDirectory(ctx, nodePath)
			allTaskUIDs = append(allTaskUIDs, uids...)
		} else {
			log.Debugf("will add index: %s", nodePath)
			nodesToAdd = append(nodesToAdd, model.SearchNode{
				Parent: parent,
				Name:   obj.GetName(),
				IsDir:  obj.IsDir(),
				Size:   obj.GetSize(),
			})
		}
	}

	if len(nodesToAdd) > 0 {
		log.Debugf("executing add for parent %s: %d nodes", parent, len(nodesToAdd))
		uids, err := tqm.m.batchIndexWithTaskUID(ctx, nodesToAdd)
		if err != nil {
			log.Errorf("failed to batch index for parent %s: %v", parent, err)
		} else {
			allTaskUIDs = append(allTaskUIDs, uids...)
		}
	}

	if len(allTaskUIDs) > 0 {
		tqm.mu.Lock()
		tqm.pendingTasks[parent] = allTaskUIDs
		tqm.mu.Unlock()
		log.Debugf("recorded %d taskUIDs for parent %s", len(allTaskUIDs), parent)
	}
}

// indexNewDirectory recursively indexes a newly created directory and all of
// its descendants (bounded by the configured max index depth).
func (tqm *TaskQueueManager) indexNewDirectory(ctx context.Context, dir string) []int64 {
	var uids []int64
	depth := calculateDepth(dir)
	limit := setting.GetInt(conf.MaxIndexDepth, 20)
	if depth > limit {
		limit = depth
	}
	err := tqm.walkNewDir(ctx, dir, limit-depth, func(nodes []model.SearchNode) {
		if len(nodes) == 0 {
			return
		}
		batchUIDs, err := tqm.m.batchIndexWithTaskUID(ctx, nodes)
		if err != nil {
			log.Errorf("failed to batch index directory %s: %v", dir, err)
			return
		}
		uids = append(uids, batchUIDs...)
	})
	if err != nil {
		log.Errorf("failed to walk new directory %s: %v", dir, err)
	}
	return uids
}

// walkNewDir walks dir up to depth levels below it and batches every visited
// node (including the directory itself) into emit.
func (tqm *TaskQueueManager) walkNewDir(ctx context.Context, dir string, depth int, emit func([]model.SearchNode)) error {
	parent, name := path.Split(dir)
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		parent = "/"
	}
	batch := []model.SearchNode{{Parent: parent, Name: name, IsDir: true, Size: 0}}

	var recurse func(p string, d int) error
	recurse = func(p string, d int) error {
		if d <= 0 {
			return nil
		}
		objs, err := tqm.listObjects(ctx, p)
		if err != nil {
			return err
		}
		for i := range objs {
			obj := objs[i]
			nodePath := path.Join(p, obj.GetName())
			batch = append(batch, model.SearchNode{
				Parent: p,
				Name:   obj.GetName(),
				IsDir:  obj.IsDir(),
				Size:   obj.GetSize(),
			})
			if obj.IsDir() {
				if err := recurse(nodePath, d-1); err != nil {
					log.Warnf("failed to walk %s: %v", nodePath, err)
				}
			}
		}
		return nil
	}

	if err := recurse(dir, depth); err != nil {
		return err
	}
	emit(batch)
	return nil
}
