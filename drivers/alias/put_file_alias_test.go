package alias

import (
	"context"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func newTestAlias(paths ...string) *Alias {
	d := &Alias{pathMap: map[string][]string{}}
	for _, p := range paths {
		k, v := getPair(p)
		if _, ok := d.pathMap[k]; !ok {
			d.rootOrder = append(d.rootOrder, k)
		}
		d.pathMap[k] = append(d.pathMap[k], v)
	}
	return d
}

func TestIsVirtualRoot(t *testing.T) {
	root := &model.Object{Name: "root", Path: "/", IsFolder: true, Mask: model.Locked}
	if !isVirtualRoot(root) {
		t.Fatal("aggregated root should be treated as virtual root")
	}
	balanced := BalancedObjs{&model.Object{Path: "/real/dir", IsFolder: true}}
	if isVirtualRoot(balanced) {
		t.Fatal("balanced root points to a real dir, must not be virtual")
	}
	if isVirtualRoot(nil) {
		t.Fatal("nil should not be virtual root")
	}
}

// 多映射（文件级别名）的根目录是虚拟目录，写同名映射文件时应命中写穿分支
func TestFileAliasTargetsMultiKey(t *testing.T) {
	d := newTestAlias("a.md:/x/a.md", "b.md:/y/b.md")
	root := &model.Object{Name: "root", Path: "/", IsFolder: true, Mask: model.Locked}

	targets, ok := d.fileAliasTargets(context.Background(), root, "a.md")
	if !ok || len(targets) != 1 || targets[0] != "/x/a.md" {
		t.Fatalf("want [\"/x/a.md\"], got %v (ok=%v)", targets, ok)
	}

	if _, ok := d.fileAliasTargets(context.Background(), root, "c.md"); ok {
		t.Fatal("name without mapping must not hit file alias branch")
	}
}

// 目录级别名（根目录是真实目录）不能被当成文件别名
func TestFileAliasTargetsDirRoot(t *testing.T) {
	d := newTestAlias("a.md:/x/a.md")
	balanced := BalancedObjs{&model.Object{Path: "/x", IsFolder: true}}
	if _, ok := d.fileAliasTargets(context.Background(), balanced, "a.md"); ok {
		t.Fatal("dir root should not hit file alias branch unless target is a file")
	}
}
