package crypt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
)

// 默认 256 MiB，跟随 OPENLIST_BENCH_SIZE_MB 调整。
// CI 上用小值（如 64），本地想看真实吞吐再调大。
var benchSize = func() int {
	if v := os.Getenv("OPENLIST_BENCH_SIZE_MB"); v != "" {
		if mb, err := strconv.Atoi(v); err == nil && mb > 0 {
			return mb << 20
		}
	}
	return 256 << 20
}()

// 懒加载：包级直接构造会让每次 go test 都白占 256MB
var benchData = sync.OnceValue(func() []byte {
	return bytes.Repeat([]byte("openlist-perf-"), benchSize/14+1)[:benchSize]
})

// 只测加密本身（不落盘），代表 CPU 上限
func BenchmarkEncryptOnly(b *testing.B) {
	d := newTestCrypt(b)
	buf := make([]byte, 1<<20)
	b.SetBytes(int64(benchSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := d.cipher.EncryptData(newTestStream(benchData()))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.CopyBuffer(io.Discard, r, buf); err != nil {
			b.Fatal(err)
		}
	}
}

// 加密 + 顺带算 SHA256（不落盘），与 BenchmarkEncryptOnly 的差值就是摘要的净成本
func BenchmarkEncryptAndHash(b *testing.B) {
	d := newTestCrypt(b)
	buf := make([]byte, 1<<20)
	hasher := sha256.New()
	b.SetBytes(int64(benchSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hasher.Reset()
		r, err := d.cipher.EncryptData(newTestStream(benchData()))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.CopyBuffer(io.MultiWriter(io.Discard, hasher), r, buf); err != nil {
			b.Fatal(err)
		}
	}
}

// 纯顺序写盘基线，用来判断瓶颈是磁盘还是加密
func BenchmarkPlainWrite(b *testing.B) {
	buf := make([]byte, 1<<20)
	b.SetBytes(int64(benchSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := os.CreateTemp(conf.Conf.TempDir, "bench-plain-*")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.CopyBuffer(f, bytes.NewReader(benchData()), buf); err != nil {
			b.Fatal(err)
		}
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
		_ = os.Remove(f.Name())
	}
}

// 实际走的那条路：边加密边写盘
func BenchmarkMaterialize(b *testing.B) {
	d := newTestCrypt(b)
	b.SetBytes(int64(benchSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench-materialize-%d", i)
		path, _, _, err := d.materializeCiphertext(context.Background(), key, "bench/a.bin", newTestStream(benchData()), nil)
		if err != nil {
			b.Fatal(err)
		}
		_ = os.Remove(path)
		dropCiphertext(key)
	}
}
