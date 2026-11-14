package crypt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	stdpath "path"
	"regexp"
	"strings"
	"sync"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	rcCrypt "github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	log "github.com/sirupsen/logrus"
)

type Crypt struct {
	model.Storage
	Addition
	cipher *rcCrypt.Cipher
}

const obfuscatedPrefix = "___Obfuscated___"

func (d *Crypt) Config() driver.Config {
	return config
}

func (d *Crypt) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Crypt) Init(ctx context.Context) error {
	// obfuscate credentials if it's updated or just created
	err := d.updateObfusParm(&d.Password)
	if err != nil {
		return fmt.Errorf("failed to obfuscate password: %w", err)
	}
	err = d.updateObfusParm(&d.Salt)
	if err != nil {
		return fmt.Errorf("failed to obfuscate salt: %w", err)
	}

	isCryptExt := regexp.MustCompile(`^[.][A-Za-z0-9-_]{2,}$`).MatchString
	if !isCryptExt(d.EncryptedSuffix) {
		return fmt.Errorf("EncryptedSuffix is Illegal")
	}
	d.FileNameEncoding = utils.GetNoneEmpty(d.FileNameEncoding, "base64")
	d.EncryptedSuffix = utils.GetNoneEmpty(d.EncryptedSuffix, ".bin")
	d.RemotePath = utils.FixAndCleanPath(d.RemotePath)

	p, _ := strings.CutPrefix(d.Password, obfuscatedPrefix)
	p2, _ := strings.CutPrefix(d.Salt, obfuscatedPrefix)
	config := configmap.Simple{
		"password":                  p,
		"password2":                 p2,
		"filename_encryption":       d.FileNameEnc,
		"directory_name_encryption": d.DirNameEnc,
		"filename_encoding":         d.FileNameEncoding,
		"suffix":                    d.EncryptedSuffix,
		"pass_bad_blocks":           "",
	}
	c, err := rcCrypt.NewCipher(config)
	if err != nil {
		return fmt.Errorf("failed to create Cipher: %w", err)
	}
	d.cipher = c

	// 没有索引文件时后台建一次全量索引；有则跳过，避免每次启动都扫盘
	d.ensureNameMapIndex()

	return nil
}

func (d *Crypt) updateObfusParm(str *string) error {
	temp := *str
	if !strings.HasPrefix(temp, obfuscatedPrefix) {
		temp, err := obscure.Obscure(temp)
		if err != nil {
			return err
		}
		temp = obfuscatedPrefix + temp
		*str = temp
	}
	return nil
}

func (d *Crypt) Drop(ctx context.Context) error {
	return nil
}

func (d *Crypt) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	remoteFullPath := dir.GetPath()
	objs, err := fs.List(ctx, remoteFullPath, &fs.ListArgs{NoLog: true, Refresh: args.Refresh})
	// the obj must implement the model.SetPath interface
	// return objs, err
	if err != nil {
		return nil, err
	}

	result := make([]model.Obj, 0, len(objs))
	for _, obj := range objs {
		size := obj.GetSize()
		mask := model.GetObjMask(obj)
		name := obj.GetName()
		if mask&model.Virtual == 0 {
			if obj.IsDir() {
				name, err = d.cipher.DecryptDirName(model.UnwrapObjName(obj).GetName())
				if err != nil {
					// filter illegal files
					continue
				}
			} else {
				size, err = d.cipher.DecryptedSize(size)
				if err != nil {
					// filter illegal files
					continue
				}
				name, err = d.cipher.DecryptFileName(model.UnwrapObjName(obj).GetName())
				if err != nil {
					// filter illegal files
					continue
				}
			}
		}
		if !d.ShowHidden && strings.HasPrefix(name, ".") {
			continue
		}
		objRes := &model.Object{
			Path:     stdpath.Join(remoteFullPath, obj.GetName()),
			Name:     name,
			Size:     size,
			Modified: obj.ModTime(),
			IsFolder: obj.IsDir(),
			Ctime:    obj.CreateTime(),
			Mask:     mask &^ model.Temp,
			// discarding hash as it's encrypted
		}
		if !d.Thumbnail || !strings.HasPrefix(args.ReqPath, "/") {
			result = append(result, objRes)
			continue
		}
		thumbPath := stdpath.Join(args.ReqPath, ".thumbnails", name+".webp")
		thumb := fmt.Sprintf("%s/d%s?sign=%s",
			common.GetApiUrl(ctx),
			utils.EncodePath(thumbPath, true),
			sign.Sign(thumbPath))
		result = append(result, &model.ObjThumb{
			Object: *objRes,
			Thumbnail: model.Thumbnail{
				Thumbnail: thumb,
			},
		})
	}

	// 记录明文名与云盘上密文名的对应关系，供对照页展示（异步落盘、自动防抖）
	d.recordNameMap(args.ReqPath, remoteActualOf(remoteFullPath), result)

	return result, nil
}

func (a Addition) GetRootPath() string {
	return a.RemotePath
}

func (d *Crypt) Get(ctx context.Context, path string) (model.Obj, error) {
	firstTryIsFolder, secondTry := guessPath(path)
	remoteFullPath := stdpath.Join(d.RemotePath, d.encryptPath(path, firstTryIsFolder))
	remoteObj, err := fs.Get(ctx, remoteFullPath, &fs.GetArgs{NoLog: true})
	if err != nil {
		if errors.Is(err, errs.StorageNotFound) {
			remoteFullPath = stdpath.Join(d.RemotePath, path)
			remoteObj, err = fs.Get(ctx, remoteFullPath, &fs.GetArgs{NoLog: true})
			if err != nil {
				// 可能是 虚拟路径+开启文件夹加密：返回NotSupport让op.Get去尝试op.List查找
				return nil, errs.NotSupport
			}
		} else if secondTry && errs.IsObjectNotFound(err) {
			// try the opposite
			remoteFullPath = stdpath.Join(d.RemotePath, d.encryptPath(path, !firstTryIsFolder))
			remoteObj, err = fs.Get(ctx, remoteFullPath, &fs.GetArgs{NoLog: true})
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	size := remoteObj.GetSize()
	name := remoteObj.GetName()
	mask := model.GetObjMask(remoteObj) &^ model.Temp
	if mask&model.Virtual == 0 {
		if !remoteObj.IsDir() {
			decryptedSize, err := d.cipher.DecryptedSize(size)
			if err != nil {
				log.Warnf("DecryptedSize failed for %s ,will use original size, err:%s", path, err)
			} else {
				size = decryptedSize
			}
			decryptedName, err := d.cipher.DecryptFileName(model.UnwrapObjName(remoteObj).GetName())
			if err != nil {
				log.Warnf("DecryptFileName failed for %s ,will use original name, err:%s", path, err)
			} else {
				name = decryptedName
			}
		} else {
			decryptedName, err := d.cipher.DecryptDirName(model.UnwrapObjName(remoteObj).GetName())
			if err != nil {
				log.Warnf("DecryptDirName failed for %s ,will use original name, err:%s", path, err)
			} else {
				name = decryptedName
			}
		}
	}
	return &model.Object{
		Path:     remoteFullPath,
		Name:     name,
		Size:     size,
		Modified: remoteObj.ModTime(),
		IsFolder: remoteObj.IsDir(),
		Ctime:    remoteObj.CreateTime(),
		Mask:     mask,
	}, nil
}

// https://github.com/rclone/rclone/blob/v1.67.0/backend/crypt/cipher.go#L37
const fileHeaderSize = 32

func (d *Crypt) Link(ctx context.Context, file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	remoteStorage, remoteActualPath, err := op.GetStorageAndActualPath(file.GetPath())
	if err != nil {
		return nil, err
	}
	remoteLink, remoteFile, err := op.Link(ctx, remoteStorage, remoteActualPath, model.LinkArgs{})
	if err != nil {
		return nil, err
	}

	remoteSize := remoteLink.ContentLength
	if remoteSize <= 0 {
		remoteSize = remoteFile.GetSize()
	}
	rrf, err := stream.GetRangeReaderFromLink(remoteSize, remoteLink)
	if err != nil {
		_ = remoteLink.Close()
		return nil, fmt.Errorf("the remote storage driver need to be enhanced to support encryption")
	}

	mu := &sync.Mutex{}
	var fileHeader []byte
	rangeReaderFunc := func(ctx context.Context, offset, limit int64) (io.ReadCloser, error) {
		length := limit
		if offset == 0 && limit > 0 {
			mu.Lock()
			if limit <= fileHeaderSize {
				defer mu.Unlock()
				if fileHeader != nil {
					return io.NopCloser(bytes.NewReader(fileHeader[:limit])), nil
				}
				length = fileHeaderSize
			} else if fileHeader == nil {
				defer mu.Unlock()
			} else {
				mu.Unlock()
			}
		}

		remoteReader, err := rrf.RangeRead(ctx, http_range.Range{Start: offset, Length: length})
		if err != nil {
			return nil, err
		}

		if offset == 0 && limit > 0 {
			fileHeader = make([]byte, fileHeaderSize)
			n, err := io.ReadFull(remoteReader, fileHeader)
			if n != fileHeaderSize {
				fileHeader = nil
				return nil, fmt.Errorf("failed to read all data: (expect =%d, actual =%d) %w", fileHeaderSize, n, err)
			}
			if limit <= fileHeaderSize {
				remoteReader.Close()
				return io.NopCloser(bytes.NewReader(fileHeader[:limit])), nil
			} else {
				remoteReader = utils.ReadCloser{
					Reader: io.MultiReader(bytes.NewReader(fileHeader), remoteReader),
					Closer: remoteReader,
				}
			}
		}
		return remoteReader, nil
	}
	return &model.Link{
		RangeReader: stream.RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
			readSeeker, err := d.cipher.DecryptDataSeek(ctx, rangeReaderFunc, httpRange.Start, httpRange.Length)
			if err != nil {
				return nil, err
			}
			return readSeeker, nil
		}),
		SyncClosers:      utils.NewSyncClosers(remoteLink),
		RequireReference: remoteLink.RequireReference,
	}, nil
}

func (d *Crypt) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	remoteStorage, remoteActualPath, err := op.GetStorageAndActualPath(parentDir.GetPath())
	if err != nil {
		return err
	}
	encryptedName := d.cipher.EncryptDirName(dirName)
	err = op.MakeDir(ctx, remoteStorage, stdpath.Join(remoteActualPath, encryptedName))
	if err == nil {
		d.refreshNameMapDir(parentDir.GetPath())
	}
	return err
}

func (d *Crypt) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	_, err := fs.Move(ctx, srcObj.GetPath(), dstDir.GetPath())
	if err == nil {
		d.refreshNameMapDir(stdpath.Dir(srcObj.GetPath()))
		d.refreshNameMapDir(dstDir.GetPath())
		d.syncThumbnailMove(ctx, srcObj, dstDir.GetPath(), false)
	}
	return err
}

func (d *Crypt) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	remoteStorage, remoteActualPath, err := op.GetStorageAndActualPath(srcObj.GetPath())
	if err != nil {
		return err
	}
	var newEncryptedName string
	if srcObj.IsDir() {
		newEncryptedName = d.cipher.EncryptDirName(newName)
	} else {
		newEncryptedName = d.cipher.EncryptFileName(newName)
	}
	err = op.Rename(ctx, remoteStorage, remoteActualPath, newEncryptedName)
	if err == nil {
		d.refreshNameMapDir(stdpath.Dir(srcObj.GetPath()))
		d.syncThumbnailRename(ctx, srcObj, newName)
	}
	return err
}

func (d *Crypt) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	_, err := fs.Copy(ctx, srcObj.GetPath(), dstDir.GetPath())
	if err == nil {
		d.refreshNameMapDir(dstDir.GetPath())
		d.syncThumbnailMove(ctx, srcObj, dstDir.GetPath(), true)
	}
	return err
}

func (d *Crypt) Remove(ctx context.Context, obj model.Obj) error {
	remoteStorage, remoteActualPath, err := op.GetStorageAndActualPath(obj.GetPath())
	if err != nil {
		return err
	}
	err = op.Remove(ctx, remoteStorage, remoteActualPath)
	if err == nil {
		d.refreshNameMapDir(stdpath.Dir(obj.GetPath()))
		d.syncThumbnailRemove(ctx, obj)
	}
	return err
}

func (d *Crypt) Put(ctx context.Context, dstDir model.Obj, streamer model.FileStreamer, up driver.UpdateProgress) error {
	remoteStorage, remoteActualPath, err := op.GetStorageAndActualPath(dstDir.GetPath())
	if err != nil {
		return err
	}

	encName := d.cipher.EncryptFileName(streamer.GetName())
	encSize := d.cipher.EncryptedSize(streamer.GetSize())

	var (
		reader  io.Reader
		cleanup func(success bool)
		// 密文的 SHA256：能拿到就一路传给底层，省掉它自己再全量读一遍算摘要
		sum string
	)
	switch s := streamer.(type) {
	case *preparedCiphertext:
		// 入队前已经加密落盘（见 PrepareUpload），这里只负责打开并上传
		f, openErr := os.Open(s.path)
		if openErr != nil {
			return fmt.Errorf("ciphertext cache of [%s] is gone (expired or swept), please upload again: %w", s.GetName(), openErr)
		}
		// 必须保持 reader 的动态类型是 *os.File。*os.File 天然满足 model.File
		// （io.ReaderAt + io.ReadSeeker），FileStream.GetFile() 的类型断言会直接命中，
		// 于是底层驱动（139 等）的 CacheFullAndHash / NewStreamSectionReader 都复用这个句柄，
		// 不会新建 HybridCache 把整份密文再读进内存。换成 bufio.Reader 之类会静默退化。
		reader, encSize, sum = f, s.size, s.sum
		cleanup = ciphertextCleanup(f, s.key)
	default:
		// 底层支持断点续传（如 139 的分片会话）、用户没关开关、且当前是可暂停的上传任务时，
		// 必须喂同一份密文：rclone crypt 每个文件随机 nonce，重新加密会得到
		// 完全不同的字节，已上传的分片就全废了。
		if d.resumableWith(remoteStorage) && ctx.Value(conf.KeepStreamKey) != nil {
			cached, cachedSize, cachedSum, cl, cacheErr := d.stableCiphertext(
				ctx, remoteStorage.GetStorage().ID, remoteActualPath, encName, streamer, up)
			if cacheErr != nil {
				return cacheErr
			}
			reader, cleanup, sum = cached, cl, cachedSum
			if cachedSize > 0 {
				encSize = cachedSize
			}
		} else {
			reader, err = d.cipher.EncryptData(streamer)
			if err != nil {
				return fmt.Errorf("failed to EncryptData: %w", err)
			}
		}
	}

	// doesn't support seekableStream, since rapid-upload is not working for encrypted data
	obj := &model.Object{
		ID:       streamer.GetID(),
		Path:     streamer.GetPath(),
		Name:     encName,
		Size:     encSize,
		Modified: streamer.ModTime(),
		IsFolder: streamer.IsDir(),
	}
	if sum != "" {
		// 摘要对应的是密文字节，也就是底层实际收到的内容。
		// 带上它，139 这类驱动会在 complete 校验里直接命中，跳过 CacheFullAndHash 的整份读取。
		obj.HashInfo = utils.NewHashInfo(utils.SHA256, sum)
	}
	streamOut := &stream.FileStream{
		Obj:               obj,
		Reader:            reader,
		Mimetype:          "application/octet-stream",
		ForceStreamUpload: true,
		Exist:             streamer.GetExist(),
	}
	err = op.Put(ctx, remoteStorage, remoteActualPath, streamOut, up)
	if cleanup != nil {
		// 上传成功就释放密文缓存；失败则保留，供下次续传复用同一份字节
		cleanup(err == nil)
	}
	if err == nil {
		// 新文件立刻进索引，不用再手动点进这个目录
		d.refreshNameMapDir(dstDir.GetPath())
	}
	return err
}

func (d *Crypt) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	remoteStorage, _, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return nil, errs.NotImplement
	}
	remoteDetails, err := op.GetStorageDetails(ctx, remoteStorage)
	if err != nil {
		return nil, err
	}
	return &model.StorageDetails{
		DiskUsage: remoteDetails.DiskUsage,
	}, nil
}

// preparedCiphertext 是「已加密落盘、只等上传」的流。外层保留明文的 FileStreamer
// （Put 里还要用它算加密后的名字/大小，并沿用 mtime 等元信息），密文以缓存文件随行。
type preparedCiphertext struct {
	model.FileStreamer
	key  string
	path string
	size int64
	// sum 是密文的 SHA256，加密时顺手算出来的。
	// 带上它，底层驱动（139 等）就不用为了算摘要再全量读一遍这份密文。
	sum string
}

// PrepareUpload 实现 driver.StreamPreparer：在任务入队前就把明文流直接加密落盘。
// 框架默认会先把整份明文缓存下来（HybridCache 内存优先），任务再拿这份明文去加密，
// 结果是「明文占内存 + 密文占磁盘」两份数据。这里把两步合成一步：读完请求体就直接
// 得到密文，明文既不落地也不进内存，上传任务只持有一份密文。
//
// 底层不支持续传时没必要落盘（流式加密就够），返回 nil 交回默认行为。
func (d *Crypt) PrepareUpload(ctx context.Context, dstDirActualPath string, streamer model.FileStreamer) (model.FileStreamer, error) {
	remote, _, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		log.Warnf("crypt: failed to resolve remote storage [%s], fall back to default stream cache: %v", d.RemotePath, err)
		return nil, nil
	}
	// 大小未知（-1）时算不出密文长度，交给默认缓存先把流落到磁盘。
	// 注意 0 字节文件是可以走这条路的：EncryptedSize(0) 返回 32 字节文件头，一样能预加密。
	if !d.resumableWith(remote) || streamer.GetSize() < 0 {
		return nil, nil
	}

	virtualPath := stdpath.Join(d.GetStorage().MountPath, dstDirActualPath, streamer.GetName())
	key := cipherCacheKey(d.GetStorage().ID, virtualPath, streamer.GetSize(), streamer.ModTime())
	// 入队前顺手清一次过期缓存：中断后没再被重试的任务，只能靠这里回收临时文件
	sweepExpired()
	path, size, sum, err := d.materializeCiphertext(ctx, key, virtualPath, streamer, nil)
	if err != nil {
		return nil, err
	}
	log.Debugf("crypt: prepared ciphertext of [%s] (%d bytes, sha256=%s) before enqueue", virtualPath, size, sum)
	return &preparedCiphertext{FileStreamer: streamer, key: key, path: path, size: size, sum: sum}, nil
}

// resumableWith 判断「经此 crypt 存储上传到 remote 能不能暂停/继续」。
func (d *Crypt) resumableWith(remote driver.Driver) bool {
	if d.DisableResumable || remote == nil {
		return false
	}
	r, ok := remote.(driver.Resumable)
	return ok && r.Resumable()
}

// Resumable 实现 driver.Resumable，供上传任务决定要不要给用户「暂停」按钮。
func (d *Crypt) Resumable() bool {
	remote, _, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return false
	}
	return d.resumableWith(remote)
}

var _ driver.Driver = (*Crypt)(nil)
