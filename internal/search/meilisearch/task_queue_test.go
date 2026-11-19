package meilisearch

import (
	"context"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func stubLister(context.Context, string) ([]model.Obj, error) { return nil, nil }

func TestCalculateDepth(t *testing.T) {
	cases := map[string]int{
		"/":            0,
		"/a":           1,
		"/a/b":         2,
		"/a/b/c.txt":   3,
		"/电影/蓝光":      2,
	}
	for p, want := range cases {
		if got := calculateDepth(p); got != want {
			t.Fatalf("calculateDepth(%q) = %d, want %d", p, got, want)
		}
	}
}

func TestEnqueueDedupByParent(t *testing.T) {
	tqm := NewTaskQueueManager(&Meilisearch{}, stubLister)
	tqm.Enqueue("/a")
	tqm.Enqueue("/a")
	tqm.Enqueue("/b")
	if len(tqm.queue) != 2 {
		t.Fatalf("queue length = %d, want 2", len(tqm.queue))
	}
}

func TestReEnqueueDoesNotOverwriteNewer(t *testing.T) {
	tqm := NewTaskQueueManager(&Meilisearch{}, stubLister)
	newer := &QueuedTask{Parent: "/a", Depth: 1, EnqueueAt: time.Now()}
	tqm.queue["/a"] = newer

	tqm.reEnqueue(&QueuedTask{Parent: "/a", Depth: 1, EnqueueAt: time.Now().Add(-time.Hour)})
	if tqm.queue["/a"] != newer {
		t.Fatal("reEnqueue overwrote a newer queued task")
	}

	tqm.Enqueue("/b")
	if _, ok := tqm.queue["/b"]; !ok {
		t.Fatal("missing parent not re-enqueued")
	}
}
