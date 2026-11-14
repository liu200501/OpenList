package crypt

import (
	stdpath "path"
	"testing"

	rcCrypt "github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/config/configmap"
)

func newTestCipherWith(t testing.TB, fileEnc, dirEnc string) *rcCrypt.Cipher {
	t.Helper()
	c, err := rcCrypt.NewCipher(configmap.Simple{
		"password":                  "openlist-test-password",
		"password2":                 "",
		"filename_encryption":       fileEnc,
		"directory_name_encryption": dirEnc,
		"filename_encoding":         "base64",
		"suffix":                    ".bin",
	})
	if err != nil {
		t.Fatalf("failed to create cipher (%s/%s): %v", fileEnc, dirEnc, err)
	}
	return c
}

// 核心不变量：同步逻辑拼出的密文路径，必须和 List 生成的缩略图 URL
// 经 crypt.Get 加密后查找的底层对象完全一致；否则同步会作用在错误文件上。
func TestThumbnailRelPathMatchesGetEncryption(t *testing.T) {
	for _, cfg := range []struct{ file, dir string }{
		{"off", "false"},
		{"off", "true"},
		{"standard", "false"},
		{"standard", "true"},
		{"obfuscate", "false"},
		{"obfuscate", "true"},
	} {
		t.Run(cfg.file+"_dir_"+cfg.dir, func(t *testing.T) {
			c := newTestCipherWith(t, cfg.file, cfg.dir)
			d := &Crypt{cipher: c}
			for _, plain := range []string{"a.mp4", "三体 2024.mkv", "no_ext"} {
				thumbPlain := plain + thumbnailExt
				rel := thumbnailRelPath(c, plain)
				dirSeg, fileSeg := stdpath.Split(rel)

				// 1) 解密回去必须是 ".thumbnails" 和 "<明文名>.webp"
				gotDir, err := c.DecryptDirName(stdpath.Base(dirSeg))
				if err != nil {
					t.Fatalf("[%s] decrypt dir segment %q: %v", plain, dirSeg, err)
				}
				if gotDir != thumbnailDirName {
					t.Errorf("[%s] dir segment = %q, want %q", plain, gotDir, thumbnailDirName)
				}
				gotFile, err := c.DecryptFileName(fileSeg)
				if err != nil {
					t.Fatalf("[%s] decrypt file segment %q: %v", plain, fileSeg, err)
				}
				if gotFile != thumbPlain {
					t.Errorf("[%s] file segment = %q, want %q", plain, gotFile, thumbPlain)
				}

				// 2) 与 crypt.Get 实际使用的加密路径末两段逐字节一致。
				// 换不同深度的父目录结果必须不变（rclone 逐段加密，不依赖上下文）。
				for _, parent := range []string{"/dir", "/加密/2024/子目录"} {
					encPath := d.encryptPath(parent+"/"+thumbnailDirName+"/"+thumbPlain, false)
					want := stdpath.Join(stdpath.Base(stdpath.Dir(encPath)), stdpath.Base(encPath))
					if rel != want {
						t.Errorf("[%s] parent %s: rel = %q, crypt.Get path tail = %q", plain, parent, rel, want)
					}
				}
			}
			if thumbnailRelPath(c, "a.mp4") == thumbnailRelPath(c, "b.mp4") {
				t.Error("different plain names must map to different thumbnail paths")
			}
		})
	}
}

// 目录对象一律跳过，保证整目录移动/复制时 .thumbnails 由底层原生搬迁带走，不回归。
func TestShouldSyncThumbnail(t *testing.T) {
	cases := []struct {
		enabled, isDir, want bool
	}{
		{true, false, true},
		{true, true, false},
		{false, false, false},
		{false, true, false},
	}
	for _, tc := range cases {
		if got := shouldSyncThumbnail(tc.enabled, tc.isDir); got != tc.want {
			t.Errorf("shouldSyncThumbnail(%t, %t) = %t, want %t", tc.enabled, tc.isDir, got, tc.want)
		}
	}
}

func TestThumbnailPathUnder(t *testing.T) {
	c := newTestCipherWith(t, "standard", "true")
	d := &Crypt{cipher: c}
	dir := "/local/加密/p7YqKG61IX7vrIBiGK4f2g"
	plain := "三体.mp4"

	got := d.thumbnailPathUnder(dir, plain)
	want := stdpath.Join(dir, thumbnailRelPath(c, plain))
	if got != want {
		t.Errorf("thumbnailPathUnder = %q, want %q", got, want)
	}
	if stdpath.Dir(got) != d.thumbnailDirUnder(dir) {
		t.Errorf("thumbnail parent = %q, want %q", stdpath.Dir(got), d.thumbnailDirUnder(dir))
	}
	if stdpath.Base(d.thumbnailDirUnder(dir)) != c.EncryptDirName(thumbnailDirName) {
		t.Error("thumbnail dir name is not the encrypted .thumbnails")
	}
}
