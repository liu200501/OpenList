package op

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetExtractStore 在测试间隔离全局缓存状态。直接整体赋值会复制 sync.Mutex，
// 触发 go vet 的 copylocks 警告，因此只交换各个字段。
func resetExtractStore(t *testing.T) {
	t.Helper()
	extractFiles.mu.Lock()
	oldEntries, oldBytes := extractFiles.entries, extractFiles.bytes
	extractFiles.entries = make(map[string]*extractedFile)
	extractFiles.bytes = 0
	extractFiles.mu.Unlock()
	t.Cleanup(func() {
		extractFiles.mu.Lock()
		extractFiles.entries, extractFiles.bytes = oldEntries, oldBytes
		extractFiles.mu.Unlock()
	})
}

func TestExtractFileStorePutGetExpire(t *testing.T) {
	dir := t.TempDir()
	resetExtractStore(t)

	p := filepath.Join(dir, "cached.bin")
	if err := os.WriteFile(p, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	extractFilePut("k", p, 4)

	e, ok := extractFileGet("k")
	if !ok {
		t.Fatal("expected cache hit after put")
	}
	if e.path != p || e.size != 4 {
		t.Fatalf("entry = %+v, want path=%s size=4", e, p)
	}
	extractFiles.mu.Lock()
	gotBytes := extractFiles.bytes
	extractFiles.mu.Unlock()
	if gotBytes != 4 {
		t.Fatalf("bytes = %d, want 4", gotBytes)
	}

	// Force expiry by moving the timestamp into the past.
	extractFiles.mu.Lock()
	extractFiles.entries["k"].expiresAt = time.Now().Add(-time.Second)
	extractFiles.mu.Unlock()
	if _, ok := extractFileGet("k"); ok {
		t.Fatal("expected miss for expired entry")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expired cache file was not removed: %v", err)
	}
	extractFiles.mu.Lock()
	gotBytes = extractFiles.bytes
	extractFiles.mu.Unlock()
	if gotBytes != 0 {
		t.Fatalf("bytes after expiry = %d, want 0", gotBytes)
	}
}

func TestExtractFileStoreOverwriteRemovesOldFile(t *testing.T) {
	dir := t.TempDir()
	resetExtractStore(t)

	p1 := filepath.Join(dir, "one.bin")
	p2 := filepath.Join(dir, "two.bin")
	_ = os.WriteFile(p1, []byte("1111"), 0o644)
	_ = os.WriteFile(p2, []byte("2222"), 0o644)

	extractFilePut("k", p1, 4)
	extractFilePut("k", p2, 4)

	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Fatalf("old cache file not removed on overwrite: %v", err)
	}
	extractFiles.mu.Lock()
	gotBytes := extractFiles.bytes
	extractFiles.mu.Unlock()
	if gotBytes != 4 {
		t.Fatalf("bytes = %d after overwrite, want 4", gotBytes)
	}
}

func TestExtractFileStoreEvictsByOldest(t *testing.T) {
	dir := t.TempDir()
	resetExtractStore(t)

	prevLimit := extractFileMaxBytes
	extractFileMaxBytes = 10
	t.Cleanup(func() { extractFileMaxBytes = prevLimit })

	for i, name := range []string{"a.bin", "b.bin", "c.bin"} {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte("xxxx"), 0o644)
		extractFilePut(name, p, 4)
		// Ensure strictly increasing expiry order.
		extractFiles.mu.Lock()
		extractFiles.entries[name].expiresAt = extractFiles.entries[name].expiresAt.Add(time.Duration(i) * time.Second)
		extractFiles.mu.Unlock()
	}
	extractFiles.mu.Lock()
	gotBytes := extractFiles.bytes
	gotLen := len(extractFiles.entries)
	extractFiles.mu.Unlock()
	if gotBytes > extractFileMaxBytes {
		t.Fatalf("bytes = %d, want <= %d after eviction", gotBytes, extractFileMaxBytes)
	}
	if gotLen == 0 {
		t.Fatal("expected some entries to remain")
	}
}

func TestExtractFileGetConcurrentSafe(t *testing.T) {
	dir := t.TempDir()
	resetExtractStore(t)

	p := filepath.Join(dir, "c.bin")
	_ = os.WriteFile(p, []byte("data"), 0o644)
	extractFilePut("k", p, 4)

	var wg sync.WaitGroup
	var hits atomic.Int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := extractFileGet("k"); ok {
				hits.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := hits.Load(); got != 50 {
		t.Fatalf("concurrent hits = %d, want 50", got)
	}
}
