package handles

import (
	"os"
	"strconv"

	"github.com/OpenListTeam/OpenList/v4/internal/namemap"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// CryptNameMap 提供 crypt 存储的「明文 ↔ 密文」对照页。
// 路径 /cryptmap/<storageID>?sign=...，签名与 /d 一致，避免明文名被匿名访问。
func CryptNameMap(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.ErrorPage(c, err, 400)
		return
	}
	if err := sign.Verify("/cryptmap/"+idStr, c.Query("sign")); err != nil {
		common.ErrorPage(c, err, 401)
		return
	}
	// 优先用内存索引实时渲染（模板一改就生效），没有索引才回落到磁盘上的静态页。
	if mount := namemap.ReadMount(uint(id)); mount != "" {
		if st, err := op.GetStorageByMountPath(mount); err == nil {
			if r, ok := st.(interface{ RescanNameMap() }); ok {
				if c.Query("rescan") == "1" {
					r.RescanNameMap()
				}
			}
			if p, ok := st.(interface{ NameMapPage() ([]byte, bool) }); ok {
				if b, ok2 := p.NameMapPage(); ok2 {
					c.Header("Content-Type", "text/html; charset=utf-8")
					c.Data(200, "text/html; charset=utf-8", b)
					return
				}
			}
		}
	}
	path := namemap.HTMLPath(uint(id))
	if _, err := os.Stat(path); err != nil {
		common.ErrorPage(c, err, 404)
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.File(path)
}
