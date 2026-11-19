package handles

import (
	"strconv"
	"strings"

	_139 "github.com/OpenListTeam/OpenList/v4/drivers/139"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// FsTransferShareReq 跨账号 139 秒转存请求。
//
// 账号 A（源）= 由目标存储的 source_storage_id 指向的另一个 139 存储；
// 账号 B（目标）= dst_path 所在的 139 存储。
//
// source_folder_ids / source_file_ids 是源账号云盘里的**内容 ID**（caID/coID），
// 可通过在源存储下列目录得到的对象 ID 取得。二者至少填一个。
type FsTransferShareReq struct {
	DstPath         string   `json:"dst_path" form:"dst_path"`
	SourceFolderIDs []string `json:"source_folder_ids" form:"source_folder_ids"`
	SourceFileIDs   []string `json:"source_file_ids" form:"source_file_ids"`
}

// FsTransferShare 跨账号分享秒转存。
//
// 流程（全程服务端，零字节上传/下载）：
//   1. 源账号 139 存储创建分享
//   2. 目标账号订阅分享
//   3. 目标账号把分享内容转存到 dst_path
//   4. 轮询任务直到完成
//
// 需要登录用户（外部脚本用全局 token 调用即可被识别为 admin）。
func FsTransferShare(c *gin.Context) {
	var req FsTransferShareReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if len(req.SourceFolderIDs) == 0 && len(req.SourceFileIDs) == 0 {
		common.ErrorStrResp(c, "source_folder_ids or source_file_ids is required", 400)
		return
	}

	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	if !user.CanCopy() {
		common.ErrorResp(c, errs.PermissionDenied, 403)
		return
	}

	// 目标路径权限校验
	dstDir, err := user.JoinPath(req.DstPath)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	dstMeta, err := op.GetNearestMeta(dstDir)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		common.ErrorResp(c, err, 500, true)
		return
	}
	if !common.CanWrite(user, dstMeta, dstDir) {
		common.ErrorResp(c, errs.PermissionDenied, 403)
		return
	}

	// 目标驱动（账号 B）
	dstDriver, dstActualPath, err := op.GetStorageAndActualPath(dstDir)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	dst139, ok := dstDriver.(*_139.Yun139)
	if !ok {
		common.ErrorStrResp(c, "destination storage is not a 139 storage", 400)
		return
	}

	// 源驱动（账号 A）：从目标存储的 source_storage_id 解析
	srcIDStr := strings.TrimSpace(dst139.Addition.SourceStorageID)
	if srcIDStr == "" {
		common.ErrorStrResp(c, "source_storage_id is not configured on destination 139 storage", 400)
		return
	}
	srcID, err := strconv.ParseUint(srcIDStr, 10, 64)
	if err != nil {
		common.ErrorStrResp(c, "invalid source_storage_id: "+srcIDStr, 400)
		return
	}
	srcModel, err := db.GetStorageById(uint(srcID))
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	srcDriver, err := op.GetStorageByMountPath(srcModel.MountPath)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	src139, ok := srcDriver.(*_139.Yun139)
	if !ok {
		common.ErrorStrResp(c, "source storage is not a 139 storage", 400)
		return
	}
	if src139.UserDomainID == "" || dst139.UserDomainID == "" {
		common.ErrorStrResp(c, "both source and destination 139 storages must have user_domain_id (ud_id) configured", 400)
		return
	}

	// 目标目录必须是 139 里的文件夹；取其对象 ID 作为 newCatalogID。
	dstObj, err := op.Get(c.Request.Context(), dstDriver, dstActualPath)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if !dstObj.IsDir() {
		common.ErrorStrResp(c, "dst_path is not a directory", 400)
		return
	}
	dstDirID := dstObj.GetID()
	if dstDirID == "" || dstDirID == "/" {
		dstDirID = "/"
	}
	dstDirName := dstObj.GetName()
	if dstDirName == "" || dstDirName == "/" {
		dstDirName = "全部文件"
	}

	result, err := dst139.SaveShareToPath(src139, req.SourceFolderIDs, req.SourceFileIDs, dstDirID, dstDirName)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, result)
}
