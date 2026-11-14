package crypt

import (
	"context"
	stdpath "path"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	rcCrypt "github.com/rclone/rclone/backend/crypt"
	log "github.com/sirupsen/logrus"
)

const (
	thumbnailDirName = ".thumbnails"
	thumbnailExt     = ".webp"
)

// thumbnailRelPath 返回缩略图相对「文件所在目录」的密文相对路径。
//
// List 按 "<当前目录>/.thumbnails/<明文文件名>.webp" 拼缩略图 URL（driver.go），
// crypt.Get 会把这段虚拟路径逐段加密后到底层查找。rclone 的名称加密是按 "/"
// 分段处理的（EncryptDirName / EncryptFileName 内部都走 encryptFileName 逐段加密，
// 空段原样返回），因此单独加密 ".thumbnails" 与 "<明文名>.webp" 得到的
// 就是同一个底层对象，不会引入第二套命名规则。
func thumbnailRelPath(cipher *rcCrypt.Cipher, plainName string) string {
	return stdpath.Join(
		cipher.EncryptDirName(thumbnailDirName),
		cipher.EncryptFileName(plainName+thumbnailExt),
	)
}

// shouldSyncThumbnail 是同步开关的纯判定：只对「开启缩略图」的单个文件生效。
// 目录对象必须跳过：整目录移动/复制由底层原生搬迁带走目录内部的 .thumbnails，
// 额外同步反而会破坏这个既有行为（见验收标准 5）。
func shouldSyncThumbnail(enabled, isDir bool) bool {
	return enabled && !isDir
}

// thumbnailPathUnder 返回缩略图在底层存储里的挂载路径。
// dirRemotePath 是文件所在目录的底层挂载路径（Get/List 里 Obj.Path 的语义）。
func (d *Crypt) thumbnailPathUnder(dirRemotePath, plainName string) string {
	return stdpath.Join(dirRemotePath, thumbnailRelPath(d.cipher, plainName))
}

func (d *Crypt) thumbnailDirUnder(dirRemotePath string) string {
	return stdpath.Join(dirRemotePath, d.cipher.EncryptDirName(thumbnailDirName))
}

// thumbnailSyncCtx 让缩略图同步始终同步完成、且不触发用户配置的对象更新 hook：
// 主文件操作已经触发过一次 hook，缩略图属于内部伴随操作；webp 通常很小，
// 用 NoTaskKey 避免底层驱动不支持原生移动/复制时退化成后台任务、导致
// 主操作返回后缩略图还没到位。
func thumbnailSyncCtx(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, conf.SkipHookKey, struct{}{})
	return context.WithValue(ctx, conf.NoTaskKey, struct{}{})
}

// thumbnailExists 探测缩略图对象是否存在。「不存在」是绝大多数文件的正常状态，
// 不记日志；其余异常记警告并当作不存在，绝不阻断主操作。
func thumbnailExists(ctx context.Context, path string) bool {
	_, err := fs.Get(ctx, path, &fs.GetArgs{NoLog: true})
	if err == nil {
		return true
	}
	if !errs.IsNotFoundError(err) && !errs.IsObjectNotFound(err) {
		log.Warnf("[crypt] thumbnail sync: probe [%s] failed, skipped: %v", path, err)
	}
	return false
}

func warnThumbnail(action, path string, err error) {
	log.Warnf("[crypt] thumbnail sync: %s [%s] failed: %v", action, path, err)
}

// syncThumbnailMove 在主文件移动/复制成功后同步 .thumbnails/<明文名>.webp。
// copyMode=true 时复制（源文件保留），否则移动（源目录不留孤儿）。
// 所有失败都只记警告，主操作已经成功的结果不回滚。
func (d *Crypt) syncThumbnailMove(ctx context.Context, srcObj model.Obj, dstDirRemotePath string, copyMode bool) {
	if !shouldSyncThumbnail(d.Thumbnail, srcObj.IsDir()) {
		return
	}
	srcDirRemote := stdpath.Dir(srcObj.GetPath())
	plainName := srcObj.GetName()
	srcThumb := d.thumbnailPathUnder(srcDirRemote, plainName)
	dstThumb := d.thumbnailPathUnder(dstDirRemotePath, plainName)
	// 同目录同名的复制没有可同步的目标，且下一步的清理会误删源缩略图。
	if srcThumb == dstThumb {
		return
	}
	ctx = thumbnailSyncCtx(ctx)
	dstThumbDir := d.thumbnailDirUnder(dstDirRemotePath)

	srcExists := thumbnailExists(ctx, srcThumb)
	// 目标已有同名 webp 会被新目录的列表当成这个文件的缩略图（串图），先清掉；
	// 源没有缩略图时也要清，否则新目录会显示成别人的图。
	if thumbnailExists(ctx, dstThumb) {
		if err := fs.Remove(ctx, dstThumb); err != nil {
			warnThumbnail("remove stale target", dstThumb, err)
		}
	}
	if !srcExists {
		return
	}
	// op.MakeDir 幂等：目录已存在直接返回，不存在时连同父目录一起建。
	if err := fs.MakeDir(ctx, dstThumbDir); err != nil {
		warnThumbnail("make target dir", dstThumbDir, err)
		return
	}

	action := "move"
	var err error
	if copyMode {
		action = "copy"
		_, err = fs.Copy(ctx, srcThumb, dstThumbDir)
	} else {
		_, err = fs.Move(ctx, srcThumb, dstThumbDir)
	}
	if err != nil {
		warnThumbnail(action, srcThumb, err)
	}
}

// syncThumbnailRename 在主文件重命名成功后把缩略图改成新明文名对应的密文名。
func (d *Crypt) syncThumbnailRename(ctx context.Context, srcObj model.Obj, newName string) {
	if !shouldSyncThumbnail(d.Thumbnail, srcObj.IsDir()) || srcObj.GetName() == newName {
		return
	}
	dirRemote := stdpath.Dir(srcObj.GetPath())
	oldThumb := d.thumbnailPathUnder(dirRemote, srcObj.GetName())
	newThumb := d.thumbnailPathUnder(dirRemote, newName)
	ctx = thumbnailSyncCtx(ctx)

	// 新名字下若已有残留 webp，重命名后会串图，先清掉。
	if thumbnailExists(ctx, newThumb) {
		if err := fs.Remove(ctx, newThumb); err != nil {
			warnThumbnail("remove stale target", newThumb, err)
		}
	}
	if !thumbnailExists(ctx, oldThumb) {
		return
	}
	// 直接改底层密文对象的名字，避免把 webp 内容解密后再加密一遍。
	if err := fs.Rename(ctx, oldThumb, d.cipher.EncryptFileName(newName+thumbnailExt)); err != nil {
		warnThumbnail("rename", oldThumb, err)
	}
}

// syncThumbnailRemove 在主文件删除成功后清掉对应缩略图。
func (d *Crypt) syncThumbnailRemove(ctx context.Context, obj model.Obj) {
	if !shouldSyncThumbnail(d.Thumbnail, obj.IsDir()) {
		return
	}
	thumb := d.thumbnailPathUnder(stdpath.Dir(obj.GetPath()), obj.GetName())
	ctx = thumbnailSyncCtx(ctx)
	if !thumbnailExists(ctx, thumb) {
		return
	}
	if err := fs.Remove(ctx, thumb); err != nil {
		warnThumbnail("remove", thumb, err)
	}
}
