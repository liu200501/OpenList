package tache

import (
	"sync/atomic"
	"testing"
	"time"
)

type pauseTestTask struct {
	Base
	runs int32
}

func (t *pauseTestTask) Run() error {
	// 第一次执行会一直阻塞（模拟正在传输），直到被暂停/取消取消掉 ctx
	if atomic.AddInt32(&t.runs, 1) == 1 {
		<-t.Ctx().Done()
		return t.Ctx().Err()
	}
	return nil
}

func waitState[T Task](t *testing.T, m *Manager[T], id string, want State, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if task, ok := m.GetByID(id); ok && task.GetState() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := State(-1)
	if task, ok := m.GetByID(id); ok {
		got = task.GetState()
	}
	t.Fatalf("state = %d, want %d", got, want)
}

func TestPauseKeepsStateAndResumeReruns(t *testing.T) {
	m := NewManager[*pauseTestTask](WithWorks(1), WithMaxRetry(0))
	task := &pauseTestTask{}
	m.Add(task)
	id := task.GetID()

	waitState(t, m, id, StateRunning, 3*time.Second)

	m.PauseTask(id)
	// 暂停必须停在 StatePaused，而不是被 worker 收尾成 canceled
	waitState(t, m, id, StatePaused, 3*time.Second)

	// 再等一会，确认没有被改写成终态
	time.Sleep(200 * time.Millisecond)
	if got := task.GetState(); got != StatePaused {
		t.Fatalf("paused task drifted to state %d", got)
	}

	m.ResumeTask(id)
	waitState(t, m, id, StateSucceeded, 3*time.Second)
	if got := atomic.LoadInt32(&task.runs); got != 2 {
		t.Fatalf("task ran %d times, want 2", got)
	}
}

func TestCancelPausedTaskGoesToCanceled(t *testing.T) {
	m := NewManager[*pauseTestTask](WithWorks(1), WithMaxRetry(0))
	task := &pauseTestTask{}
	m.Add(task)
	id := task.GetID()

	waitState(t, m, id, StateRunning, 3*time.Second)
	m.PauseTask(id)
	waitState(t, m, id, StatePaused, 3*time.Second)

	m.Cancel(id)
	waitState(t, m, id, StateCanceled, 3*time.Second)
}
