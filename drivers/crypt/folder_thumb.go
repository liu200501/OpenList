package crypt

import (
	"context"
	"fmt"
	stdpath "path"
	"sort"
	"strings"
	"sync"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	log "github.com/sirupsen/logrus"
)

const (
	// folderThumbMaxDirs 限制单次列目录最多为多少个文件夹探测封面。
	// 每探测一个文件夹都要额外读一次底层目录，不加限制时含上千子目录的
	// 目录会触发上千次云端 API 调用。
	folderThumbMaxDirs = 50
	// folderThumbConcurrency 是并发探测上限，用来压住 N+1 列目录的串行延迟。
	folderThumbConcurrency = 4
)

// folderThumbImageExts 是浏览器 <img> 能直接渲染的图片扩展名白名单。
// 有意排除 tif/tiff/heic/heif/swf：浏览器普遍无法直接渲染，选作封面只会得到空白。
// 这里用固定白名单而不是后台 image_types 设置，保证选图规则可枚举、可单测。
var folderThumbImageExts = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".webp": {},
	".bmp": {}, ".svg": {}, ".ico": {}, ".avif": {},
}

// isFolderThumbImage 判断一个明文文件名能否作为文件夹封面。
func isFolderThumbImage(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return false
	}
	_, ok := folderThumbImageExts[strings.ToLower(stdpath.Ext(name))]
	return ok
}

// firstFolderThumbImage 从明文文件名列表中选出「第一张图片」。
// 规则固定：只保留白名单扩展名；先按文件名小写升序，小写相同再按原名
// 字节序升序，取第一个。与底层存储、加密配置、列目录返回顺序都无关。
func firstFolderThumbImage(names []string) (string, bool) {
	candidates := make([]string, 0, len(names))
	for _, n := range names {
		if isFolderThumbImage(n) {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := strings.ToLower(candidates[i]), strings.ToLower(candidates[j])
		if a != b {
			return a < b
		}
		return candidates[i] < candidates[j]
	})
	return candidates[0], true
}

func folderThumbScanCount(n int) int {
	if n < 0 {
		return 0
	}
	if n > folderThumbMaxDirs {
		return folderThumbMaxDirs
	}
	return n
}

// folderThumbVirtualPath 返回文件夹封面在 crypt 虚拟路径下的明文路径。
// useWebp=true 时指向该图片预生成的 .thumbnails/<明文名>.webp，否则指向原图。
// 路径全程用明文，/d 会经 crypt.Get 逐段加密后到底层取对象，再由 crypt.Link 解密。
func folderThumbVirtualPath(reqDir, folderName, imageName string, useWebp bool) string {
	if useWebp {
		return stdpath.Join(reqDir, folderName, thumbnailDirName, imageName+thumbnailExt)
	}
	return stdpath.Join(reqDir, folderName, imageName)
}

// signedDownURL 与 List 中既有缩略图 URL 使用完全相同的构造方式。
func signedDownURL(apiURL, virtualPath string) string {
	return fmt.Sprintf("%s/d%s?sign=%s",
		apiURL, utils.EncodePath(virtualPath, true), sign.Sign(virtualPath))
}

// folderThumbExists 探测「预生成的 webp」是否存在。缺失是常态，不记日志；
// 其它异常只记 Debug，封面是装饰性能力，任何异常都退化成「不存在」。
func folderThumbExists(ctx context.Context, path string) bool {
	_, err := fs.Get(ctx, path, &fs.GetArgs{NoLog: true})
	if err == nil {
		return true
	}
	if !errs.IsNotFoundError(err) && !errs.IsObjectNotFound(err) {
		log.Debugf("[crypt] folder thumbnail: probe [%s] failed: %v", path, err)
	}
	return false
}

// firstFolderImageName 读取一个密文子目录，解密出明文文件名后挑第一张图片。
// SkipHook=true：这是内部探测，不能触发用户配置的对象更新 hook（搜索索引 / strm）。
func (d *Crypt) firstFolderImageName(ctx context.Context, dirRemotePath string, refresh bool) (string, bool) {
	children, err := fs.List(ctx, dirRemotePath, &fs.ListArgs{
		NoLog: true, Refresh: refresh, SkipHook: true,
	})
	if err != nil {
		log.Debugf("[crypt] folder thumbnail: list [%s] failed, skipped: %v", dirRemotePath, err)
		return "", false
	}
	names := make([]string, 0, len(children))
	for _, child := range children {
		if child.IsDir() || model.GetObjMask(child)&model.Virtual != 0 {
			continue
		}
		name, err := d.cipher.DecryptFileName(model.UnwrapObjName(child).GetName())
		if err != nil {
			// 底层目录里混进了非 crypt 文件，不是本功能能处理的对象。
			continue
		}
		names = append(names, name)
	}
	return firstFolderThumbImage(names)
}

type folderThumbTarget struct {
	thumb         *model.ObjThumb
	dirRemotePath string
	folderName    string
}

// folderCoverURL 返回文件夹封面的 URL；返回空串表示保留 List 里已生成的 URL。
// 优先级：预置的「父目录/.thumbnails/<文件夹名>.webp」> 首图自己的 webp > 首图原图。
func (d *Crypt) folderCoverURL(ctx context.Context, apiURL, reqDir, parentRemotePath string,
	thumbEnabled, refresh bool, t folderThumbTarget) string {
	if thumbEnabled && folderThumbExists(ctx, d.thumbnailPathUnder(parentRemotePath, t.folderName)) {
		// 用户手工生成的文件夹封面优先，不做覆盖。
		return ""
	}
	imageName, ok := d.firstFolderImageName(ctx, t.dirRemotePath, refresh)
	if !ok {
		return ""
	}
	useWebp := thumbEnabled && folderThumbExists(ctx, d.thumbnailPathUnder(t.dirRemotePath, imageName))
	return signedDownURL(apiURL, folderThumbVirtualPath(reqDir, t.folderName, imageName, useWebp))
}

// fillFolderThumbnails 并发解析封面并写回各自的 ObjThumb。
// 每个 goroutine 只写自己那个 target 的字符串字段，互不共享内存。
func (d *Crypt) fillFolderThumbnails(ctx context.Context, apiURL, reqDir, parentRemotePath string,
	thumbEnabled, refresh bool, targets []folderThumbTarget) {
	n := folderThumbScanCount(len(targets))
	if len(targets) > n {
		log.Debugf("[crypt] folder thumbnail: %d dirs in [%s], only first %d scanned",
			len(targets), reqDir, n)
	}
	targets = targets[:n]
	workers := folderThumbConcurrency
	if workers > len(targets) {
		workers = len(targets)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				t := targets[i]
				if u := d.folderCoverURL(ctx, apiURL, reqDir, parentRemotePath, thumbEnabled, refresh, t); u != "" {
					t.thumb.Thumbnail.Thumbnail = u
				}
			}
		}()
	}
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}
