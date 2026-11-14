package crypt

import (
	stdpath "path"
	"testing"
)

func TestIsFolderThumbImage(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"a.jpg", true},
		{"a.JPG", true},
		{"照片 2024.PNG", true},
		{"cover.webp", true},
		{"cover.avif", true},
		{"icon.svg", true},
		{"icon.ico", true},
		{"scan.gif", true},
		{"scan.bmp", true},
		{"a.jpeg", true},
		{"", false},
		{".hidden.jpg", false},
		{"a.txt", false},
		{"a.jpg.bak", false},
		{"a.tiff", false},
		{"a.tif", false},
		{"a.heic", false},
		{"a.heif", false},
		{"a.swf", false},
		{"no_ext", false},
	}
	for _, tc := range cases {
		if got := isFolderThumbImage(tc.name); got != tc.want {
			t.Errorf("isFolderThumbImage(%q) = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestFirstFolderThumbImage(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  string
		ok    bool
	}{
		{"no files", nil, "", false},
		{"no image", []string{"readme.txt", "movie.mkv", ".hidden.png"}, "", false},
		{"single", []string{"readme.txt", "DSCF0001.JPG"}, "DSCF0001.JPG", true},
		{
			name:  "case insensitive name order",
			names: []string{"zeta.txt", "b.png", "C.GIF", "a.jpg", "B.PNG"},
			want:  "a.jpg",
			ok:    true,
		},
		{
			// 小写相同的时候按原名（字节序）决定，不能依赖底层返回顺序。
			name:  "stable tie break",
			names: []string{"a.jpg", "A.JPG"},
			want:  "A.JPG",
			ok:    true,
		},
		{
			name:  "order independent of listing",
			names: []string{"Z.webp", "10.png", "2.png"},
			want:  "10.png",
			ok:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := firstFolderThumbImage(tc.names)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("firstFolderThumbImage(%v) = (%q,%t), want (%q,%t)",
					tc.names, got, ok, tc.want, tc.ok)
			}
			// 打乱输入顺序结果必须不变。
			reversed := make([]string, len(tc.names))
			for i, n := range tc.names {
				reversed[len(tc.names)-1-i] = n
			}
			got2, ok2 := firstFolderThumbImage(reversed)
			if got2 != got || ok2 != ok {
				t.Errorf("listing order changed result: %v -> (%q,%t)", reversed, got2, ok2)
			}
		})
	}
}

func TestFolderThumbVirtualPath(t *testing.T) {
	cases := []struct {
		reqDir, folder, image string
		useWebp               bool
		want                  string
	}{
		{"/crypt/photos", "相册", "a.jpg", false, "/crypt/photos/相册/a.jpg"},
		{"/crypt/photos", "相册", "a.jpg", true, "/crypt/photos/相册/.thumbnails/a.jpg.webp"},
		{"/", "summer", "IMG 001.PNG", false, "/summer/IMG 001.PNG"},
		{"/", "summer", "IMG 001.PNG", true, "/summer/.thumbnails/IMG 001.PNG.webp"},
	}
	for _, tc := range cases {
		got := folderThumbVirtualPath(tc.reqDir, tc.folder, tc.image, tc.useWebp)
		if got != tc.want {
			t.Errorf("folderThumbVirtualPath(%q,%q,%q,%t) = %q, want %q",
				tc.reqDir, tc.folder, tc.image, tc.useWebp, got, tc.want)
		}
		if stdpath.IsAbs(got) != true {
			t.Errorf("virtual path must stay absolute: %q", got)
		}
	}
}

func TestFolderThumbScanCount(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, 0}, {0, 0}, {1, 1}, {49, 49}, {50, 50},
		{51, folderThumbMaxDirs}, {1000, folderThumbMaxDirs},
	}
	for _, tc := range cases {
		if got := folderThumbScanCount(tc.in); got != tc.want {
			t.Errorf("folderThumbScanCount(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
