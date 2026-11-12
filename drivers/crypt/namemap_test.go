package crypt

import (
	"strings"
	"testing"
)

func TestRenderNameMap(t *testing.T) {
	items := []nameMapEntry{
		{Plain: "电影", Raw: "P7YqKG61IX7vrIBiGK4f2g", PlainPath: "/电影", RawPath: "/加密/P7YqKG61IX7vrIBiGK4f2g", Dir: true},
		{Plain: "2024", Raw: "poT66lxDggnVgzYbz9f-9A", PlainPath: "/电影/2024", RawPath: "/加密/P7YqKG61IX7vrIBiGK4f2g/poT66lxDggnVgzYbz9f-9A", Dir: true},
		{Plain: "三体.mp4", Raw: "O0og6rsLuYWU_7i0Pjv9BQ", PlainPath: "/电影/2024/三体.mp4", RawPath: "/加密/P7YqKG61IX7vrIBiGK4f2g/poT66lxDggnVgzYbz9f-9A/O0og6rsLuYWU_7i0Pjv9BQ", Size: 2048},
	}
	html := renderNameMap(41, "/加密存储", items)

	for _, want := range []string{"明文 ↔ 密文对照", "/加密存储", "电影", "2024", "三体.mp4", "P7YqKG61IX7vrIBiGK4f2g", "O0og6rsLuYWU_7i0Pjv9BQ", "共 2 个目录、1 个文件"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing %q", want)
		}
	}
	// 按明文路径排序：父目录行必须排在子项之前
	parent := strings.Index(html, `data-q="/电影 `)
	child := strings.Index(html, `data-q="/电影/2024 `)
	if parent < 0 || child < 0 || parent > child {
		t.Fatalf("rows are not sorted by plain path (parent=%d child=%d)", parent, child)
	}
}
