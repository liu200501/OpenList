package crypt

import (
	"context"
	"fmt"
	"net/url"
	"os"
	stdpath "path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
)

const (
	// 虚拟路径 /crypt_fixture/photos 对应底层真实目录 <mount>/<密文 photos>。
	folderThumbReqDir   = "/crypt_fixture/photos"
	folderThumbParent   = "photos"
	folderThumbMount    = "/crypt_fixture"
	folderThumbTestHost = "http://api.test"
)

// folderThumbListCtx 模拟一次真实的 HTTP 列目录请求所需的上下文值。
func folderThumbListCtx() context.Context {
	return context.WithValue(context.Background(), conf.ApiUrlKey, folderThumbTestHost)
}

func (f *thumbnailFixture) parentEnc() string {
	return f.d.cipher.EncryptDirName(folderThumbParent)
}

func (f *thumbnailFixture) listFolderThumb(t *testing.T) []model.Obj {
	t.Helper()
	if err := os.MkdirAll(f.phys(f.parentEnc()), 0o755); err != nil {
		t.Fatal(err)
	}
	objs, err := f.d.List(folderThumbListCtx(),
		&model.Object{Path: f.remote(f.parentEnc()), Name: folderThumbParent, IsFolder: true},
		model.ListArgs{ReqPath: folderThumbReqDir})
	if err != nil {
		t.Fatalf("Crypt.List failed: %v", err)
	}
	return objs
}

// writeFolder 按 crypt 的密文命名规则，在 <photos>/<folder> 下写入文件。
func (f *thumbnailFixture) writeFolder(t *testing.T, folder string, plainFiles ...string) {
	t.Helper()
	encDir := f.d.cipher.EncryptDirName(folder)
	if err := os.MkdirAll(f.phys(f.parentEnc(), encDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range plainFiles {
		writeFixture(t, f.phys(f.parentEnc(), encDir, f.d.cipher.EncryptFileName(name)), strings.Repeat("x", 64))
	}
}

func folderThumbOf(t *testing.T, objs []model.Obj, name string) string {
	t.Helper()
	for _, o := range objs {
		if o.GetName() == name {
			thumb, _ := model.GetThumb(o)
			return thumb
		}
	}
	t.Fatalf("object %q not found in list (%d objs)", name, len(objs))
	return ""
}

// assertFolderThumb 校验 URL 的路径部分、签名，以及 URL 经 crypt.Get 加密后
// 确实落在预期的底层密文对象上（避免出现「测试通过但线上 404」的假绿）。
func assertFolderThumb(t *testing.T, f *thumbnailFixture, thumb, wantPath string, wantEncrypted ...string) {
	t.Helper()
	prefix := folderThumbTestHost + "/d" + wantPath + "?sign="
	if !strings.HasPrefix(thumb, prefix) {
		t.Fatalf("thumb = %q, want prefix %q", thumb, prefix)
	}
	u, err := url.Parse(thumb)
	if err != nil {
		t.Fatal(err)
	}
	if err := sign.Verify(wantPath, u.Query().Get("sign")); err != nil {
		t.Errorf("sign of %q is invalid: %v", wantPath, err)
	}
	if len(wantEncrypted) > 0 {
		rel := strings.TrimPrefix(wantPath, folderThumbMount)
		got := stdpath.Join(f.mount, f.d.encryptPath(rel, false))
		if got != f.remote(wantEncrypted...) {
			t.Errorf("URL %q resolves to %q, want %q", wantPath, got, f.remote(wantEncrypted...))
		}
		if _, err := os.Stat(filepathFromRemote(f, got)); err != nil {
			t.Errorf("URL %q points to a missing object %q: %v", wantPath, got, err)
		}
	}
}

// filepathFromRemote 把底层挂载路径换回本机真实文件路径。
func filepathFromRemote(f *thumbnailFixture, remotePath string) string {
	rel := strings.TrimPrefix(remotePath, f.mount)
	return f.phys(filepath.FromSlash(strings.TrimPrefix(rel, "/")))
}

func TestFolderThumbFirstImageOriginalURL(t *testing.T) {
	f := newThumbnailFixture(t)
	f.d.Addition.FolderThumbnail = true
	f.writeFolder(t, "album", "z.jpg", "readme.txt", "b.png", "a.jpg")

	objs := f.listFolderThumb(t)
	want := stdpath.Join(folderThumbReqDir, "album", "a.jpg")
	assertFolderThumb(t, f, folderThumbOf(t, objs, "album"), want,
		f.parentEnc(), f.d.cipher.EncryptDirName("album"), f.d.cipher.EncryptFileName("a.jpg"))
}

func TestFolderThumbPrefersPreGeneratedImageWebp(t *testing.T) {
	f := newThumbnailFixture(t)
	f.d.Addition.FolderThumbnail = true
	f.writeFolder(t, "album", "b.jpg", "a.jpg")
	encDir := f.d.cipher.EncryptDirName("album")
	writeFixture(t, f.phys(f.parentEnc(), encDir, f.thumbDir(), f.thumbFile("a.jpg")), "webp")

	objs := f.listFolderThumb(t)
	want := stdpath.Join(folderThumbReqDir, "album", thumbnailDirName, "a.jpg"+thumbnailExt)
	assertFolderThumb(t, f, folderThumbOf(t, objs, "album"), want,
		f.parentEnc(), encDir, f.thumbDir(), f.thumbFile("a.jpg"))
}

func TestFolderThumbExplicitCoverWins(t *testing.T) {
	f := newThumbnailFixture(t)
	f.d.Addition.FolderThumbnail = true
	f.writeFolder(t, "album", "a.jpg")
	// 父目录下预生成的 <文件夹名>.webp 是用户显式指定的封面。
	writeFixture(t, f.phys(f.parentEnc(), f.thumbDir(), f.thumbFile("album")), "explicit")

	objs := f.listFolderThumb(t)
	want := stdpath.Join(folderThumbReqDir, thumbnailDirName, "album"+thumbnailExt)
	assertFolderThumb(t, f, folderThumbOf(t, objs, "album"), want,
		f.parentEnc(), f.thumbDir(), f.thumbFile("album"))
}

func TestFolderThumbNoImageKeepsFallback(t *testing.T) {
	f := newThumbnailFixture(t)
	f.d.Addition.FolderThumbnail = true
	f.writeFolder(t, "album", "readme.txt", "movie.mkv")

	objs := f.listFolderThumb(t)
	assertFolderThumb(t, f, folderThumbOf(t, objs, "album"),
		stdpath.Join(folderThumbReqDir, thumbnailDirName, "album"+thumbnailExt))
}

func TestFolderThumbUndecryptableChildrenAreIgnored(t *testing.T) {
	f := newThumbnailFixture(t)
	f.d.Addition.FolderThumbnail = true
	encDir := f.d.cipher.EncryptDirName("album")
	// 未按 crypt 规则命名的文件：解密失败必须被跳过，不能让列目录报错。
	writeFixture(t, f.phys(f.parentEnc(), encDir, "plain-not-encrypted.jpg"), strings.Repeat("x", 64))

	objs := f.listFolderThumb(t)
	assertFolderThumb(t, f, folderThumbOf(t, objs, "album"),
		stdpath.Join(folderThumbReqDir, thumbnailDirName, "album"+thumbnailExt))
}

func TestFolderThumbDisabledKeepsExistingBehavior(t *testing.T) {
	f := newThumbnailFixture(t)
	f.writeFolder(t, "album", "a.jpg")

	objs := f.listFolderThumb(t)
	assertFolderThumb(t, f, folderThumbOf(t, objs, "album"),
		stdpath.Join(folderThumbReqDir, thumbnailDirName, "album"+thumbnailExt))
}

func TestFolderThumbWorksWithoutThumbnailSwitch(t *testing.T) {
	f := newThumbnailFixture(t)
	f.d.Addition.Thumbnail = false
	f.d.Addition.FolderThumbnail = true
	f.writeFolder(t, "album", "a.jpg")
	// 父目录里放一个普通文件，验证 Thumbnail=false 时文件对象仍不带 thumb。
	if err := os.MkdirAll(f.phys(f.parentEnc()), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, f.phys(f.parentEnc(), f.d.cipher.EncryptFileName("top.bin")), strings.Repeat("x", 64))

	objs := f.listFolderThumb(t)
	assertFolderThumb(t, f, folderThumbOf(t, objs, "album"),
		stdpath.Join(folderThumbReqDir, "album", "a.jpg"))
	var topObj model.Obj
	for _, o := range objs {
		if o.GetName() == "top.bin" {
			topObj = o
			break
		}
	}
	if topObj == nil {
		t.Fatal("top.bin not found in list")
	}
	if thumb, ok := model.GetThumb(topObj); ok && thumb != "" {
		t.Errorf("file must have no thumb when Thumbnail=false, got %q", thumb)
	}
}

func TestFolderThumbScanCapIsEnforced(t *testing.T) {
	f := newThumbnailFixture(t)
	f.d.Addition.FolderThumbnail = true
	const total = folderThumbMaxDirs + 1
	for i := 0; i < total; i++ {
		f.writeFolder(t, fmt.Sprintf("dir%02d", i), "a.jpg")
	}

	objs := f.listFolderThumb(t)
	cover, fallback := 0, 0
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("dir%02d", i)
		thumb := folderThumbOf(t, objs, name)
		switch {
		case strings.Contains(thumb, "/"+name+"/a.jpg?sign="):
			cover++
		case strings.Contains(thumb, "/"+name+thumbnailExt+"?sign="):
			fallback++
		default:
			t.Errorf("unexpected thumb for %s: %q", name, thumb)
		}
	}
	if cover != folderThumbMaxDirs || fallback != 1 {
		t.Errorf("cover=%d fallback=%d, want %d/%d", cover, fallback, folderThumbMaxDirs, 1)
	}
}
