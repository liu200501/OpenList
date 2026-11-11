package tache

import (
	"container/list"
	"context"
	"fmt"
	"log"
	"sync"
)

// Worker is the worker to execute task
type Worker[T Task] struct {
	ID int
}

func isCanceled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// Execute executes the task
func (w Worker[T]) Execute(task T) {
	onError := func(err error) {
		// A user pause cancels the context as well, but the task must stay in
		// StatePaused instead of turning into "canceled"/"failed": it is still
		// resumable, so neither OnFailed nor the retry bookkeeping should run.
		if task.GetState() == StatePaused {
			task.SetErr(nil)
			return
		}
		task.SetErr(err)
		finalState := StateFailed
		if isCanceled(task.Ctx()) {
			finalState = StateCanceled
			task.SetState(StateCanceled)
		} else {
			task.SetState(StateErrored)
		}
		if !needRetry(task) {
			if hook, ok := Task(task).(OnFailed); ok {
				task.SetState(StateFailing)
				hook.OnFailed()
			}
			task.SetState(finalState)
		}
	}
	// Retry immediately in the same worker until success or max retry exhausted
	for {
		func() {
			defer func() {
				if err := recover(); err != nil {
					log.Printf("error [%s] while run task [%s],stack trace:\n%s", err, task.GetID(), getCurrentGoroutineStack())
					onError(NewErr(fmt.Sprintf("panic: %v", err)))
				}
			}()

			if isRetry(task) {
				task.SetState(StateBeforeRetry)
				if hook, ok := Task(task).(OnBeforeRetry); ok {
					hook.OnBeforeRetry()
				}
			}

			task.SetState(StateRunning)
			err := task.Run()
			if err != nil {
				onError(err)
				return
			}

			task.SetState(StateSucceeded)
			if onSucceeded, ok := Task(task).(OnSucceeded); ok {
				onSucceeded.OnSucceeded()
			}
			task.SetErr(nil)
		}()

		if task.GetState() != StateWaitingRetry {
			return
		}
	}
}

// WorkerPool is the pool of workers
type WorkerPool[T Task] struct {
	workers      *list.List
	workersMutex sync.Mutex
	working      int64
	numCreated   int64
	numActive    int64
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool[T Task](size int) *WorkerPool[T] {
	workers := list.New()
	for i := 0; i < size; i++ {
		workers.PushBack(&Worker[T]{
			ID: i,
		})
	}
	return &WorkerPool[T]{
		workers:    workers,
		numCreated: int64(size),
		numActive:  int64(size),
	}
}

// Get gets a worker from pool
func (wp *WorkerPool[T]) Get() *Worker[T] {
	wp.workersMutex.Lock()
	defer wp.workersMutex.Unlock()
	if wp.working >= wp.numActive {
		return nil
	}
	wp.working += 1
	if wp.workers.Len() > 0 {
		ret := wp.workers.Front().Value.(*Worker[T])
		wp.workers.Remove(wp.workers.Front())
		return ret
	}
	ret := &Worker[T]{
		ID: int(wp.numCreated),
	}
	wp.numCreated += 1
	return ret
}

// Put puts a worker back to pool
func (wp *WorkerPool[T]) Put(worker *Worker[T]) {
	wp.workersMutex.Lock()
	defer wp.workersMutex.Unlock()
	wp.workers.PushBack(worker)
	wp.working -= 1
}

func (wp *WorkerPool[T]) SetNumActive(active int64) {
	wp.workersMutex.Lock()
	defer wp.workersMutex.Unlock()
	wp.numActive = active
}
