package crypt

// 本文件提供把 crypt 虚拟路径解析为底层 139 云盘对象（驱动 + 内容 ID）的能力，
// 供跨账号服务端秒转存使用。解析过程只做元数据查询，不读取文件字节。
//
// 关键点：绝不能复用 Crypt.Get 的返回值——它构造的 model.Object 没有填充 ID。
// 必须用 encryptPath 得到底层全路径，再对底层全路径调 fs.Get，
// 这样 GetStorageAndActualPath 才会命中底层 139 存储，拿到的对象带真实 139 内容 ID。

import (
	"context"
	stdpath "path"

	_139 "github.com/OpenListTeam/OpenList/v4/drivers/139"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// Resolved139 描述一个 crypt 虚拟路径在底层 139 存储中的身份。
type Resolved139 struct {
	Driver         *_139.Yun139 // 底层 139 驱动实例
	ID             string       // 139 内容 ID（文件的 FileId，目录的 FileId/CatalogID）
	IsDir          bool         // true=目录，false=文件
	Name           string       // 底层对象名（密文名，如 xxx.bin），用于转存的 newCatalogName
	StorageID      uint         // 底层 139 存储的 ID
	RemoteFullPath string       // 底层完整虚拟路径（调试用）
}

// ResolveBase139 返回该 crypt 存储底层的 139 驱动（不做对象解析）。
func (d *Crypt) ResolveBase139() (*_139.Yun139, error) {
	remoteStorage, _, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return nil, err
	}
	yun, ok := remoteStorage.(*_139.Yun139)
	if !ok {
		return nil, errs.NotSupport
	}
	return yun, nil
}

// Resolve139Object 把 crypt 虚拟路径解析为底层 139 驱动 + 内容 ID + 是否目录。
// path 是 crypt 存储下的明文虚拟路径（如 "/某目录/某文件"）。
func (d *Crypt) Resolve139Object(ctx context.Context, path string) (*Resolved139, error) {
	path = utils.FixAndCleanPath(path)

	// 1) 从 d.RemotePath 解析底层存储。RemotePath 是带完整挂载前缀的虚拟路径，
	//    例如 "/移动云盘/加密区"，GetStorageAndActualPath 会按最长挂载匹配命中底层 139。
	yun, err := d.ResolveBase139()
	if err != nil {
		return nil, err
	}

	// 2) 按与 Crypt.Get 一致的 guessPath 逻辑，枚举目录/文件两种候选。
	//    直接对底层全路径 fs.Get，绕过 Crypt.Get（它会丢 ID）。
	firstTryIsFolder, secondTry := guessPath(path)
	tries := []bool{firstTryIsFolder}
	if secondTry {
		tries = append(tries, !firstTryIsFolder)
	}

	var lastErr error
	for _, isFolder := range tries {
		remoteFullPath := stdpath.Join(d.RemotePath, d.encryptPath(path, isFolder))
		remoteObj, err := fs.Get(ctx, remoteFullPath, &fs.GetArgs{NoLog: true})
		if err != nil {
			lastErr = err
			continue
		}
		id := remoteObj.GetID()
		if id == "" {
			lastErr = errs.ObjectNotFound
			continue
		}
		return &Resolved139{
			Driver:         yun,
			ID:             id,
			IsDir:          remoteObj.IsDir(),
			Name:           remoteObj.GetName(),
			StorageID:      yun.GetStorage().ID,
			RemoteFullPath: remoteFullPath,
		}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errs.ObjectNotFound
}
