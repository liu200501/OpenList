package handles

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestToObjsRespEncryptedName(t *testing.T) {
	t.Run("crypt fills basename of raw path", func(t *testing.T) {
		obj := &model.Object{
			Path: "/enc/P7YqKG61IX7vrIBiGK4f2g/O0og6rsLuYWU_7i0Pjv9BQ",
			Name: "三体.mp4",
		}
		resp := toObjsResp([]model.Obj{obj}, "/crypt", false, true)
		if len(resp) != 1 {
			t.Fatalf("got %d resp, want 1", len(resp))
		}
		if got, want := resp[0].EncryptedName, "O0og6rsLuYWU_7i0Pjv9BQ"; got != want {
			t.Fatalf("EncryptedName = %q, want %q", got, want)
		}
		if got, want := resp[0].Name, "三体.mp4"; got != want {
			t.Fatalf("Name = %q, want %q", got, want)
		}
	})

	t.Run("non-crypt does not fill encrypted name", func(t *testing.T) {
		obj := &model.Object{Path: "/plain/三体.mp4", Name: "三体.mp4"}
		resp := toObjsResp([]model.Obj{obj}, "/plain", false)
		if len(resp) != 1 {
			t.Fatalf("got %d resp, want 1", len(resp))
		}
		if got := resp[0].EncryptedName; got != "" {
			t.Fatalf("EncryptedName = %q, want empty when not requested", got)
		}
	})

	t.Run("explicit false does not fill", func(t *testing.T) {
		obj := &model.Object{Path: "/enc/xxx", Name: "a.jpg"}
		resp := toObjsResp([]model.Obj{obj}, "/crypt", false, false)
		if got := resp[0].EncryptedName; got != "" {
			t.Fatalf("EncryptedName = %q, want empty", got)
		}
	})

	t.Run("wrapped name still uses underlying path", func(t *testing.T) {
		inner := &model.Object{
			Path: "/enc/ABCD1234",
			Name: "cipher",
		}
		wrapped := &model.ObjWrapName{Name: "明文名字.txt", Obj: inner}
		resp := toObjsResp([]model.Obj{wrapped}, "/crypt", false, true)
		if got, want := resp[0].Name, "明文名字.txt"; got != want {
			t.Fatalf("display Name = %q, want %q", got, want)
		}
		if got, want := resp[0].EncryptedName, "ABCD1234"; got != want {
			t.Fatalf("EncryptedName = %q, want %q", got, want)
		}
	})

	t.Run("virtual object has empty encrypted name", func(t *testing.T) {
		obj := &model.Object{Path: "/enc/VIRTUAL", Name: "virtual", Mask: model.Virtual}
		resp := toObjsResp([]model.Obj{obj}, "/crypt", false, true)
		if got := resp[0].EncryptedName; got != "" {
			t.Fatalf("EncryptedName = %q, want empty for virtual object", got)
		}
	})

	t.Run("empty path has empty encrypted name", func(t *testing.T) {
		obj := &model.Object{Name: "no-path"}
		resp := toObjsResp([]model.Obj{obj}, "/crypt", false, true)
		if got := resp[0].EncryptedName; got != "" {
			t.Fatalf("EncryptedName = %q, want empty for empty path", got)
		}
	})
}
