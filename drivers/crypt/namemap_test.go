package crypt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderNameMap(t *testing.T) {
	items := []nameMapEntry{
		{Plain: "电影", Raw: "P7YqKG61IX7vrIBiGK4f2g", PlainPath: "/电影", RawPath: "/加密/P7YqKG61IX7vrIBiGK4f2g", Dir: true},
		{Plain: "2024", Raw: "poT66lxDggnVgzYbz9f-9A", PlainPath: "/电影/2024", RawPath: "/加密/P7YqKG61IX7vrIBiGK4f2g/poT66lxDggnVgzYbz9f-9A", Dir: true},
		{Plain: "三体.mp4", Raw: "O0og6rsLuYWU_7i0Pjv9BQ", PlainPath: "/电影/2024/三体.mp4", RawPath: "/加密/P7YqKG61IX7vrIBiGK4f2g/poT66lxDggnVgzYbz9f-9A/O0og6rsLuYWU_7i0Pjv9BQ", Size: 2048},
	}
	page := renderNameMap(41, "/加密存储", items)

	for _, want := range []string{"明文 ↔ 密文", "/加密存储", "2 个目录 / 1 个文件", "重建索引"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing %q", want)
		}
	}

	const head = `<script id="nm" type="application/json">`
	i := strings.Index(page, head)
	if i < 0 {
		t.Fatal("embedded json not found")
	}
	j := strings.Index(page[i:], "</script>")
	if j < 0 {
		t.Fatal("embedded json not closed")
	}
	var list []nameMapItem
	if err := json.Unmarshal([]byte(page[i+len(head):i+j]), &list); err != nil {
		t.Fatalf("embedded json invalid: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 items, got %d", len(list))
	}
	// 按明文路径排序
	if list[0].P != "/电影" || list[1].P != "/电影/2024" || list[2].P != "/电影/2024/三体.mp4" {
		t.Fatalf("unexpected order: %+v", list)
	}
	if !list[0].D || list[2].D {
		t.Fatal("dir flag not preserved")
	}
}
