package indexwatch

import (
	"path/filepath"
	"testing"
)

func TestParseRules(t *testing.T) {
	raw := `[
		{"enabled":true,"driver":"139Yun","path":"/cmcc/追剧","interval":600,"depth":2},
		{"enabled":false,"driver":"Local","path":"/local/x","interval":5,"depth":0},
		{"enabled":true,"driver":"Crypt","path":"/crypt/电影"},
		{"enabled":true,"path":""}
	]`
	rules := parseRules(raw)
	if len(rules) != 2 {
		t.Fatalf("want 2 enabled rules with path, got %d", len(rules))
	}
	if rules[0].Driver != "139Yun" || rules[0].Interval != 600 || rules[0].Depth != 2 {
		t.Fatalf("unexpected first rule: %+v", rules[0])
	}
	// interval 缺省应补 60
	if rules[1].Interval != 60 {
		t.Fatalf("default interval should be 60, got %d", rules[1].Interval)
	}
}

func TestParseRulesInvalidJSON(t *testing.T) {
	if got := parseRules("not json"); len(got) != 0 {
		t.Fatalf("invalid json should yield no rules, got %d", len(got))
	}
}

func TestToVirtualDir(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "data", "local", "shows")
	lw := &localWatcher{rootAbs: root, w: &watcher{rule: Rule{Path: "/local/shows"}}}
	cases := []struct {
		real string
		want string
	}{
		{root, "/local/shows"},
		{filepath.Join(root, "Season1"), "/local/shows/Season1"},
		{filepath.Join(filepath.VolumeName(root), string(filepath.Separator), "data", "local", "other"), ""}, // 同盘但不在监听范围
	}
	for _, c := range cases {
		if got := lw.toVirtualDir(c.real); got != c.want {
			t.Errorf("toVirtualDir(%q) = %q, want %q", c.real, got, c.want)
		}
	}
}
