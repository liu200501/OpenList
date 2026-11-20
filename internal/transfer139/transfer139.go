package transfer139

// 本包实现 internal/fs 的"服务端秒转存"钩子，让中国移动云盘（139）之间
// （包括各自套的 crypt 加密存储）在复制时走云端服务端分享转存，零字节下载。
//
// 只在 internal/fs/transfer() 命中时构造后台任务执行，条件不满足一律回退字节流。

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	_139 "github.com/OpenListTeam/OpenList/v4/drivers/139"
	"github.com/OpenListTeam/OpenList/v4/drivers/crypt"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	log "github.com/sirupsen/logrus"
)

const serverCopyTag = "139 跨账号秒转存"

func init() {
	fs.RegisterServerCopyHook(tryServerCopy)
}

// Resolved 描述一个虚拟路径穿透 crypt 后在底层 139 的身份。
// 供 API handler 等外部调用方使用。
type Resolved struct {
	Yun139   *_139.Yun139 // 该路径所属的底层 139 驱动
	ID       string       // 139 内容 ID
	IsDir    bool
	Name     string // 底层对象名
	StorageID uint  // 底层 139 存储 ID
	ViaCrypt bool
}

// resolved 是内部别名，保持内部代码简洁。
type resolved = Resolved

// Resolve 把任意虚拟路径（裸 139 或 crypt 包装）解析为底层 139 身份。
// 不满足条件（不是 139、没有 ID、share 类型等）时返回 error。
func Resolve(ctx context.Context, virtualPath string, storage driver.Driver, actualPath string) (*Resolved, error) {
	return resolve(ctx, virtualPath, storage, actualPath)
}

// UnderlyingYun139 取存储对应的底层 139 驱动（裸 139 直接返回；crypt 返回其底层 139）。
// 不做对象/ID 解析，供已有 139 内容 ID 的调用方使用。
func UnderlyingYun139(storage driver.Driver) (*_139.Yun139, error) {
	switch d := storage.(type) {
	case *_139.Yun139:
		return d, nil
	case *crypt.Crypt:
		rr, err := d.ResolveBase139()
		if err != nil {
			return nil, err
		}
		return rr, nil
	default:
		return nil, fmt.Errorf("storage is not 139 or crypt-wrapped 139")
	}
}

// resolve 把任意虚拟路径（裸 139 或 crypt 包装）解析为底层 139 身份。
// 不满足条件（不是 139、没有 ID、share 类型等）时返回 error，触发回退。
func resolve(ctx context.Context, virtualPath string, storage driver.Driver, actualPath string) (*resolved, error) {
	var r *resolved

	switch d := storage.(type) {
	case *_139.Yun139:
		if d.IsShareType() {
			return nil, fmt.Errorf("139 share storage is not a transfer endpoint")
		}
		obj, err := op.Get(ctx, d, actualPath)
		if err != nil {
			return nil, err
		}
		id := obj.GetID()
		if id == "" || strings.Contains(id, "|") { // share 编码 ID 含自定义分隔符，排除
			return nil, fmt.Errorf("empty or invalid 139 object id")
		}
		r = &resolved{Yun139: d, ID: id, IsDir: obj.IsDir(), Name: obj.GetName(), StorageID: d.GetStorage().ID}
	case *crypt.Crypt:
		// 穿透 crypt：解析到底层 139 驱动和内容 ID（只做元数据查询，不下载字节）。
		// actualPath 是 op 层剥离 crypt 挂载点后的路径，即 crypt 虚拟路径。
		rr, err := d.Resolve139Object(ctx, actualPath)
		if err != nil {
			return nil, err
		}
		if rr.Driver.IsShareType() {
			return nil, fmt.Errorf("underlying 139 share storage is not a transfer endpoint")
		}
		r = &resolved{Yun139: rr.Driver, ID: rr.ID, IsDir: rr.IsDir, Name: rr.Name,
			StorageID: rr.StorageID, ViaCrypt: true}
	default:
		return nil, fmt.Errorf("storage is not 139 or crypt-wrapped 139")
	}
	return r, nil
}

// tryServerCopy 是注册给 internal/fs 的钩子。
func tryServerCopy(
	ctx context.Context,
	taskType string,
	srcObjPath, dstDirPath, dstName string,
	srcStorage driver.Driver, srcObjActualPath string,
	dstStorage driver.Driver, dstDirActualPath string,
) (bool, fs.ServerCopyRunner, string) {
	// 仅处理 copy；move/merge 回退。
	if taskType != "copy" || dstName != "" {
		return false, nil, ""
	}

	src, err := resolve(ctx, srcObjPath, srcStorage, srcObjActualPath)
	if err != nil {
		log.Debugf("[transfer139] resolve src skipped: %v", err)
		return false, nil, ""
	}
	dst, err := resolve(ctx, dstDirPath, dstStorage, dstDirActualPath)
	if err != nil {
		log.Debugf("[transfer139] resolve dst skipped: %v", err)
		return false, nil, ""
	}
	if !dst.IsDir {
		log.Debugf("[transfer139] dst is not a directory, fallback")
		return false, nil, ""
	}

	// 必须是两个不同的 139 账号/存储，避免自己分享给自己。
	if src.StorageID == dst.StorageID {
		return false, nil, ""
	}
	// 安全检查：目标存储必须显式配置 source_storage_id 指向源存储。
	configured := strings.TrimSpace(dst.Yun139.SourceStorageID)
	if configured == "" {
		return false, nil, ""
	}
	want, err := strconv.ParseUint(configured, 10, 64)
	if err != nil || uint(want) != src.StorageID {
		log.Debugf("[transfer139] source_storage_id (%s) does not match src storage id (%d), fallback",
			configured, src.StorageID)
		return false, nil, ""
	}
	// 两端都要求配置 user_domain_id（ud_id），否则 139 接口会失败。
	if src.Yun139.UserDomainID == "" || dst.Yun139.UserDomainID == "" {
		return false, nil, ""
	}

	// 组织源 ID：目录→folder，文件→file。
	var folderIDs, fileIDs []string
	if src.IsDir {
		folderIDs = []string{src.ID}
	} else {
		fileIDs = []string{src.ID}
	}
	srcYun, dstYun := src.Yun139, dst.Yun139
	dstDirID, dstDirName := dst.ID, dst.Name
	if dstDirID == "" || dstDirID == "/" {
		dstDirID = "/"
	}
	if dstDirName == "" || dstDirName == "/" {
		dstDirName = "全部文件"
	}

	run := func(runCtx context.Context, progress func(int)) error {
		// 用一个不受请求取消影响的上下文执行远端调用。
		base := context.Background()
		if runCtx != nil {
			base = context.WithoutCancel(runCtx)
		}
		progress(10)
		result, err := dstYun.SaveShareToPath(srcYun, folderIDs, fileIDs, dstDirID, dstDirName)
		if err != nil {
			return fmt.Errorf("139 server-side transfer failed: %w", err)
		}
		progress(100)
		// 刷新目标目录缓存，让前端立即看到转存结果（失败不影响主流程）。
		_, _ = op.List(base, dstStorage, dstDirActualPath, model.ListArgs{Refresh: true})
		log.Infof("[transfer139] server-side transfer done: task=%s link=%s folders=%d files=%d (viaCrypt src=%v dst=%v)",
			result.TaskID, result.LinkID, result.FolderCount, result.FileCount, src.ViaCrypt, dst.ViaCrypt)
		return nil
	}
	return true, run, serverCopyTag
}
