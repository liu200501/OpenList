package tache

import "context"

// Base is the base struct for all tasks to implement TaskBase interface
type Base struct {
	ID       string `json:"id"`
	State    State  `json:"state"`
	Retry    int    `json:"retry"`
	MaxRetry int    `json:"max_retry"`

	progress float64
	err      error
	ctx      context.Context
	cancel   context.CancelFunc
	persist  func()
}

func (b *Base) SetProgress(progress float64) {
	b.progress = progress
	b.Persist()
}

func (b *Base) GetProgress() float64 {
	return b.progress
}

func (b *Base) SetState(state State) {
	b.State = state
	b.Persist()
}

func (b *Base) GetState() State {
	return b.State
}

func (b *Base) GetID() string {
	return b.ID
}

func (b *Base) SetID(id string) {
	b.ID = id
	b.Persist()
}

func (b *Base) SetErr(err error) {
	b.err = err
	b.Persist()
}

func (b *Base) GetErr() error {
	return b.err
}

func (b *Base) CtxDone() <-chan struct{} {
	return b.Ctx().Done()
}

func (b *Base) SetCtx(ctx context.Context) {
	b.ctx = ctx
}

func (b *Base) SetCancelFunc(cancelFunc context.CancelFunc) {
	b.cancel = cancelFunc
}

func (b *Base) GetRetry() (int, int) {
	return b.Retry, b.MaxRetry
}

func (b *Base) SetRetry(retry int, maxRetry int) {
	b.Retry, b.MaxRetry = retry, maxRetry
}

func (b *Base) Cancel() {
	switch b.State {
	case StateSucceeded, StateCanceled, StateFailed:
		return
	case StatePaused:
		// 已暂停的任务没有 worker 在跑，canceling 中间态永远等不到收尾，
		// 直接落地为已取消。
		b.SetState(StateCanceled)
		b.SetErr(context.Canceled)
		return
	}
	if !isCanceled(b.ctx) {
		b.SetState(StateCanceling)
		b.cancel()
	}
}

func (b *Base) Ctx() context.Context {
	return b.ctx
}

// Pause stops the running task but keeps it resumable.
// The state is switched to StatePaused *before* cancelling the context, so the
// worker can tell a user pause apart from a real cancel.
func (b *Base) Pause() {
	switch b.State {
	case StateSucceeded, StateCanceled, StateFailed:
		return
	}
	if b.State == StatePaused {
		return
	}
	b.SetState(StatePaused)
	if b.cancel != nil && !isCanceled(b.ctx) {
		b.cancel()
	}
}

// IsPaused reports whether the task was paused by the user.
func (b *Base) IsPaused() bool {
	return b.State == StatePaused
}

func (b *Base) Persist() {
	if b.persist != nil {
		b.persist()
	}
}

func (b *Base) SetPersist(persist func()) {
	b.persist = persist
}

var _ TaskBase = (*Base)(nil)
