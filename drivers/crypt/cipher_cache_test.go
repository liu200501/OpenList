package crypt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	rcCrypt "github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/config/configmap"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
)

func TestMain(m *testing.M) {
	// 正常运行由 bootstrap 初始化全局配置，这里只需要一个临时目录
	if conf.Conf == nil {
		conf.Conf = &conf.Config{TempDir: os.TempDir()}
	}
	os.Exit(m.Run())
}

func newTestCrypt(t testing.TB) *Crypt {
	t.Helper()
	c, err := rcCrypt.NewCipher(configmap.Simple{
		"password":                  "openlist-test-password",
		"password2":                 "",
		"filename_encryption":       "standard",
		"directory_name_encryption": "true",
		"filename_encoding":         "base64",
		"suffix":                    ".bin",
	})
	if err != nil {
		t.Fatalf("failed to create cipher: %v", err)
	}
	return &Crypt{cipher: c}
}

// 密文落盘必须是完整可解密的，且大小与 EncryptedSize 一致（139 按这个长度分片）。
func TestMaterializeCiphertext(t *testing.T) {
	d := newTestCrypt(t)
	plain := bytes.Repeat([]byte("openlist-crypt"), 1024*32+7)
	up := func(float64) {}

	key := "test-materialize"
	path, size, sum, err := d.materializeCiphertext(context.Background(), key, "test/a.txt", newTestStream(plain), up)
	if err != nil {
		t.Fatalf("materialize failed: %v", err)
	}
	t.Cleanup(func() { dropCiphertext(key) })

	if expect := d.cipher.EncryptedSize(int64(len(plain))); size != expect {
		t.Fatalf("ciphertext size mismatch: got %d, want %d", size, expect)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("ciphertext file missing: %v", err)
	}
	if stat.Size() != size {
		t.Fatalf("file size mismatch: got %d, want %d", stat.Size(), size)
	}

	// 顺带算出的 SHA256 必须等于密文文件的真实摘要：
	// 139 会拿它做 complete 校验，算错就是上传失败。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read ciphertext: %v", err)
	}
	expectSum := sha256.Sum256(raw)
	if sum != hex.EncodeToString(expectSum[:]) {
		t.Fatalf("ciphertext sha256 mismatch: got %s, want %x", sum, expectSum)
	}

	// 读回来解密，必须与明文一致
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open ciphertext: %v", err)
	}
	defer f.Close()
	dr, err := d.cipher.DecryptData(f)
	if err != nil {
		t.Fatalf("failed to decrypt: %v", err)
	}
	defer dr.Close()
	got, err := io.ReadAll(dr)
	if err != nil {
		t.Fatalf("failed to read decrypted data: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decrypted data mismatch: got %d bytes, want %d", len(got), len(plain))
	}
}

// 同一 key 应该能反复打开（暂停 / 重试靠它），删除后不可再取。
func TestOpenAndDropCiphertext(t *testing.T) {
	d := newTestCrypt(t)
	plain := []byte("resume me")

	key := "test-open"
	path, size, sum, err := d.materializeCiphertext(context.Background(), key, "test/b.txt", newTestStream(plain), nil)
	if err != nil {
		t.Fatalf("materialize failed: %v", err)
	}

	// 句柄已关闭：可以连续打开两次，模拟两次上传尝试
	for i := 0; i < 2; i++ {
		f, gotSize, gotSum, ok := openCiphertext(key)
		if !ok {
			t.Fatalf("attempt %d: cached ciphertext not reusable", i+1)
		}
		if gotSize != size {
			t.Fatalf("attempt %d: size mismatch: got %d, want %d", i+1, gotSize, size)
		}
		if gotSum != sum {
			t.Fatalf("attempt %d: sha256 mismatch: got %s, want %s", i+1, gotSum, sum)
		}
		_ = f.Close()
	}

	dropCiphertext(key)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ciphertext file should be removed, stat err = %v", err)
	}
	if _, _, _, ok := openCiphertext(key); ok {
		t.Fatal("dropped ciphertext should not be openable")
	}
}

// 上下文取消时不应该留下临时文件。
func TestMaterializeAbortOnContextCancel(t *testing.T) {
	d := newTestCrypt(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	key := "test-abort"
	if _, _, _, err := d.materializeCiphertext(ctx, key, "test/c.txt", newTestStream([]byte("aborted")), nil); err == nil {
		t.Fatal("expected context error")
	}
	if _, _, _, ok := openCiphertext(key); ok {
		t.Fatal("aborted ciphertext should not be cached")
	}
}

// 0 字节文件也要能预加密：密文只有 32 字节文件头，大小与 EncryptedSize(0) 一致。
func TestMaterializeEmptyFile(t *testing.T) {
	d := newTestCrypt(t)
	key := "test-empty"
	path, size, sum, err := d.materializeCiphertext(context.Background(), key, "test/empty.txt", newTestStream(nil), nil)
	if err != nil {
		t.Fatalf("materialize empty file failed: %v", err)
	}
	t.Cleanup(func() { dropCiphertext(key) })

	if want := d.cipher.EncryptedSize(0); size != want {
		t.Fatalf("empty ciphertext size mismatch: got %d, want %d", size, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read ciphertext: %v", err)
	}
	if int64(len(raw)) != size {
		t.Fatalf("file size mismatch: got %d, want %d", len(raw), size)
	}
	expectSum := sha256.Sum256(raw)
	if sum != hex.EncodeToString(expectSum[:]) {
		t.Fatalf("empty ciphertext sha256 mismatch: got %s, want %x", sum, expectSum)
	}
}

func newTestStream(data []byte) model.FileStreamer {
	return &stream.FileStream{
		Obj:    &model.Object{Name: "test.dat", Size: int64(len(data))},
		Reader: bytes.NewReader(data),
	}
}
