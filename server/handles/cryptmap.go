package handles

import (
	"os"
	"strconv"

	"github.com/OpenListTeam/OpenList/v4/internal/namemap"
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
	path := namemap.HTMLPath(uint(id))
	if _, err := os.Stat(path); err != nil {
		common.ErrorPage(c, err, 404)
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.File(path)
}
