package handles

import (
	"errors"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/transfer139"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// FsTransferShareReq 跨账号 139 秒转存请求。
//
// 账号 A（源）= 由目标存储（或其底层 139）的 source_storage_id 指向的另一个 139 存储；
// 账号 B（目标）= dst_path 所在的 139 存储（可套 crypt）。
//
// source_folder_ids / source_file_ids 是源账号云盘里的**内容 ID**（caID/coID），
// 可通过在源存储下列目录得到的对象 ID 取得。二者至少填一个。
type FsTransferShareReq struct {
	DstPath         string   `json:"dst_path" form:"dst_path"`
	SourceFolderIDs []string `json:"source_folder_ids" form:"source_folder_ids"`
	SourceFileIDs   []string `json:"source_file_ids" form:"source_file_ids"`
}

// FsTransferShare 跨账号 139 秒转存。
//
// 目标 dst_path 可以是裸 139 存储，也可以是套在 139 上的 crypt 存储；
// 源的 source_storage_id 指向的存储同样支持这两种形态。全程在云端完成，零字节下载。
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

	// 目标路径权限校验（基于虚拟路径）
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

	// 目标：解析到裸 139 或 crypt 底层的 139（含目录 ID / 名称）。
	dstDriver, dstActualPath, err := op.GetStorageAndActualPath(dstDir)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	dstResolved, err := transfer139.Resolve(c.Request.Context(), dstDir, dstDriver, dstActualPath)
	if err != nil {
		common.ErrorStrResp(c, "destination is not a 139 (or crypt-wrapped 139) storage: "+err.Error(), 400)
		return
	}
	if !dstResolved.IsDir {
		common.ErrorStrResp(c, "dst_path is not a directory", 400)
		return
	}
	dst139 := dstResolved.Yun139

	// 源：从目标 139 的 source_storage_id 解析源存储，再取底层 139（支持 crypt 包装的源存储）。
	srcIDStr := strings.TrimSpace(dst139.SourceStorageID)
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
	src139, err := transfer139.UnderlyingYun139(srcDriver)
	if err != nil {
		common.ErrorStrResp(c, "source storage is not a 139 (or crypt-wrapped 139) storage: "+err.Error(), 400)
		return
	}
	if src139.IsShareType() {
		common.ErrorStrResp(c, "source 139 share storage cannot create shares", 400)
		return
	}
	if src139.UserDomainID == "" || dst139.UserDomainID == "" {
		common.ErrorStrResp(c, "both source and destination 139 storages must have user_domain_id (ud_id) configured", 400)
		return
	}

	dstDirID, dstDirName := dstResolved.ID, dstResolved.Name
	if dstDirID == "" || dstDirID == "/" {
		dstDirID = "/"
	}
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
