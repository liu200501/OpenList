package crypt

import (
	"context"
	"fmt"
	"os"
	stdpath "path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

var (
	thumbnailDBOnce   sync.Once
	thumbnailMountSeq atomic.Uint64
)

type thumbnailFixture struct {
	d     *Crypt
	root  string
	mount string
}

// newThumbnailFixture 注册一个真实 Local 存储作为 crypt 的底层存储，
// 目录名/文件名直接当密文用：同步逻辑只认 Obj.Path 与明文 Obj.Name。
func newThumbnailFixture(t *testing.T) *thumbnailFixture {
	t.Helper()
	thumbnailDBOnce.Do(func() {
		g, err := gorm.Open(sqlite.Open("file:crypt_thumb_test?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			panic(err)
		}
		db.Init(g)
	})
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig(t.TempDir())
	t.Cleanup(func() { conf.Conf = oldConf })
	if err := os.MkdirAll(conf.Conf.TempDir, 0o755); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	mount := fmt.Sprintf("/tcrypt_%03d", thumbnailMountSeq.Add(1))
	addition, err := utils.Json.MarshalToString(map[string]string{"root_folder_path": root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: addition,
	}); err != nil {
		t.Fatalf("failed to create local storage: %v", err)
	}

	d := &Crypt{
		Storage:  model.Storage{ID: uint(90000 + thumbnailMountSeq.Load()), MountPath: "/crypt_fixture"},
		Addition: Addition{RemotePath: mount, Thumbnail: true},
		cipher:   newTestCipherWith(t, "standard", "true"),
	}
	return &thumbnailFixture{d: d, root: root, mount: mount}
}

func (f *thumbnailFixture) phys(parts ...string) string {
	return filepath.Join(append([]string{f.root}, parts...)...)
}

func (f *thumbnailFixture) remote(parts ...string) string {
	return stdpath.Join(append([]string{f.mount}, parts...)...)
}

func (f *thumbnailFixture) thumbDir() string {
	return f.d.cipher.EncryptDirName(thumbnailDirName)
}

func (f *thumbnailFixture) thumbFile(plain string) string {
	return f.d.cipher.EncryptFileName(plain + thumbnailExt)
}

func (f *thumbnailFixture) srcObj(plain string) *model.Object {
	return &model.Object{Path: f.remote("SRC", "FILE"), Name: plain}
}

func (f *thumbnailFixture) dstDir() *model.Object {
	return &model.Object{Path: f.remote("DST"), Name: "DST", IsFolder: true}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected file %s: %v", path, err)
	}
	if want != "" && string(got) != want {
		t.Errorf("%s content = %q, want %q", path, got, want)
	}
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be gone, stat err = %v", path, err)
	}
}

// 验收 1：移动单个文件后缩略图到新目录，源目录无孤儿。
func TestThumbnailSyncOnMove(t *testing.T) {
	f := newThumbnailFixture(t)
	writeFixture(t, f.phys("SRC", "FILE"), "ciphertext")
	writeFixture(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")
	if err := os.MkdirAll(f.phys("DST"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := f.d.Move(context.Background(), f.srcObj("a.mp4"), f.dstDir()); err != nil {
		t.Fatalf("Move failed: %v", err)
	}
	requireFile(t, f.phys("DST", "FILE"), "ciphertext")
	requireAbsent(t, f.phys("SRC", "FILE"))
	requireFile(t, f.phys("DST", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")
	requireAbsent(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")))
}

// 验收 2：重命名后缩略图跟随新文件名。
func TestThumbnailSyncOnRename(t *testing.T) {
	f := newThumbnailFixture(t)
	writeFixture(t, f.phys("SRC", "FILE"), "ciphertext")
	writeFixture(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")

	if err := f.d.Rename(context.Background(), f.srcObj("a.mp4"), "b.mp4"); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
	requireFile(t, f.phys("SRC", f.d.cipher.EncryptFileName("b.mp4")), "ciphertext")
	requireAbsent(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")))
	requireFile(t, f.phys("SRC", f.thumbDir(), f.thumbFile("b.mp4")), "webp-src")
}

// 验收 2：删除后缩略图一并清理。
func TestThumbnailSyncOnRemove(t *testing.T) {
	f := newThumbnailFixture(t)
	writeFixture(t, f.phys("SRC", "FILE"), "ciphertext")
	writeFixture(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")

	if err := f.d.Remove(context.Background(), f.srcObj("a.mp4")); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	requireAbsent(t, f.phys("SRC", "FILE"))
	requireAbsent(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")))
}

// 复制：源保留，目标生成对应缩略图。NoTaskKey 让 Local 驱动退化出的复制任务同步执行。
func TestThumbnailSyncOnCopy(t *testing.T) {
	f := newThumbnailFixture(t)
	writeFixture(t, f.phys("SRC", "FILE"), "ciphertext")
	writeFixture(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")
	if err := os.MkdirAll(f.phys("DST"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), conf.NoTaskKey, struct{}{})

	if err := f.d.Copy(ctx, f.srcObj("a.mp4"), f.dstDir()); err != nil {
		t.Fatalf("Copy failed: %v", err)
	}
	requireFile(t, f.phys("DST", "FILE"), "ciphertext")
	requireFile(t, f.phys("SRC", "FILE"), "ciphertext")
	requireFile(t, f.phys("DST", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")
	requireFile(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")
}

// 验收 3：目标已有同名 webp 时不串图，显示的必须是源文件自己的缩略图。
func TestThumbnailSyncReplacesStaleTarget(t *testing.T) {
	f := newThumbnailFixture(t)
	writeFixture(t, f.phys("SRC", "FILE"), "ciphertext")
	writeFixture(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")
	writeFixture(t, f.phys("DST", f.thumbDir(), f.thumbFile("a.mp4")), "stale")

	if err := f.d.Move(context.Background(), f.srcObj("a.mp4"), f.dstDir()); err != nil {
		t.Fatalf("Move failed: %v", err)
	}
	requireFile(t, f.phys("DST", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")
}

// 验收 4：缩略图同步失败（目标 .thumbnails 位置被普通文件占住）不影响主操作成功。
func TestThumbnailSyncFailureDoesNotFailMainMove(t *testing.T) {
	f := newThumbnailFixture(t)
	writeFixture(t, f.phys("SRC", "FILE"), "ciphertext")
	writeFixture(t, f.phys("SRC", f.thumbDir(), f.thumbFile("a.mp4")), "webp-src")
	writeFixture(t, f.phys("DST", f.thumbDir()), "not-a-directory")

	if err := f.d.Move(context.Background(), f.srcObj("a.mp4"), f.dstDir()); err != nil {
		t.Fatalf("Move must succeed even if thumbnail sync fails: %v", err)
	}
	requireFile(t, f.phys("DST", "FILE"), "ciphertext")
}

// 验收 5：整目录移动不回归——目录内部的 .thumbnails 由底层原生搬迁带走，
// 同步逻辑不会在目标目录额外创建以目录名命名的缩略图。
func TestThumbnailSyncSkipsDirectory(t *testing.T) {
	f := newThumbnailFixture(t)
	writeFixture(t, f.phys("SRC", "INNER"), "inner")
	writeFixture(t, f.phys("SRC", f.thumbDir(), f.thumbFile("x.mp4")), "webp-inner")
	if err := os.MkdirAll(f.phys("DST"), 0o755); err != nil {
		t.Fatal(err)
	}
	srcDirObj := &model.Object{Path: f.remote("SRC"), Name: "电影", IsFolder: true}

	if err := f.d.Move(context.Background(), srcDirObj, f.dstDir()); err != nil {
		t.Fatalf("Move dir failed: %v", err)
	}
	requireFile(t, f.phys("DST", "SRC", "INNER"), "inner")
	requireFile(t, f.phys("DST", "SRC", f.thumbDir(), f.thumbFile("x.mp4")), "webp-inner")
	requireAbsent(t, f.phys("DST", f.thumbDir(), f.thumbFile("电影")))
}
