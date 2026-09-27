package lanzou

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

var upClient *resty.Client
var once sync.Once

func (d *LanZou) doupload(callback base.ReqCallback, resp interface{}) ([]byte, error) {
	return d.post(d.BaseUrl+"/doupload.php", func(req *resty.Request) {
		req.SetQueryParams(map[string]string{
			"uid": d.uid,
			"vei": d.vei,
		})
		if callback != nil {
			callback(req)
		}
	}, resp)
}

func (d *LanZou) get(url string, callback base.ReqCallback) ([]byte, error) {
	return d.request(url, http.MethodGet, callback, false)
}

func (d *LanZou) post(url string, callback base.ReqCallback, resp interface{}) ([]byte, error) {
	data, err := d._post(url, callback, resp, false)
	if err == ErrCookieExpiration && d.IsAccount() {
		if d.flag.CompareAndSwap(0, 1) {
			_, err2 := d.Login()
			d.flag.Swap(0)
			if err2 != nil {
				err = errors.Join(err, err2)
				d.Status = err.Error()
				op.MustSaveDriverStorage(d)
				return data, err
			}
		}
		for d.flag.Load() != 0 {
			runtime.Gosched()
		}
		return d._post(url, callback, resp, false)
	}
	return data, err
}

func (d *LanZou) _post(url string, callback base.ReqCallback, resp interface{}, up bool) ([]byte, error) {
	data, err := d.request(url, http.MethodPost, func(req *resty.Request) {
		req.AddRetryCondition(func(r *resty.Response, err error) bool {
			if utils.Json.Get(r.Body(), "zt").ToInt() == 4 {
				time.Sleep(time.Second)
				return true
			}
			return false
		})
		if callback != nil {
			callback(req)
		}
	}, up)
	if err != nil {
		return data, err
	}
	switch utils.Json.Get(data, "zt").ToInt() {
	case 1, 2, 4:
		if resp != nil {
			utils.Json.Unmarshal(data, resp)
		}
		return data, nil
	case 9: // 登录过期
		return data, ErrCookieExpiration
	default:
		info := utils.Json.Get(data, "inf").ToString()
		if info == "" {
			info = utils.Json.Get(data, "info").ToString()
		}
		return data, fmt.Errorf(info)
	}
}

func (d *LanZou) request(url string, method string, callback base.ReqCallback, up bool) ([]byte, error) {
	var req *resty.Request
	var vs string
	for retry := 0; retry < 5; retry++ {
		if up {
			once.Do(func() {
				upClient = base.NewRestyClient().SetTimeout(120 * time.Second)
			})
			req = upClient.R()
		} else {
			req = base.RestyClient.R()
		}

		req.SetHeaders(map[string]string{
			"Referer":    "https://pc.woozooo.com",
			"User-Agent": d.UserAgent,
		})

		if strings.Contains(url, "/file/") {
			cookie := d.Cookie
			if cookie != "" {
				cookie += "; "
			}
			cookie += "down_ip=1"
			if vs != "" {
				cookie += "; acw_sc__v2=" + vs
			}
			req.SetHeader("cookie", cookie)
		} else if d.Cookie != "" {
			cookie := d.Cookie
			if vs != "" {
				cookie += "; acw_sc__v2=" + vs
			}
			req.SetHeader("cookie", cookie)
		} else if vs != "" {
			req.SetHeader("cookie", "acw_sc__v2="+vs)
		}

		if callback != nil {
			callback(req)
		}

		res, err := req.Execute(method, url)
		if err != nil {
			return nil, err
		}
		bodyStr := res.String()
		log.Debugf("lanzou request: url=>%s ,stats=>%d ,body => %s\n", res.Request.URL, res.StatusCode(), bodyStr)
		if strings.Contains(bodyStr, "acw_sc__v2") {
			vs, err = CalcAcwScV2(bodyStr)
			if err != nil {
				return nil, err
			}
			continue
		}
		return res.Body(), err
	}
	return nil, errors.New("acw_sc__v2 validation error")
}

func (d *LanZou) Login() ([]*http.Cookie, error) {
	var vs string
	for retry := 0; retry < 3; retry++ {
		req := base.NewRestyClient().SetRedirectPolicy(resty.NoRedirectPolicy()).R()

		if vs != "" {
			req.SetHeader("cookie", "acw_sc__v2="+vs)
		}

		resp, err := req.SetFormData(map[string]string{
			"task":         "3",
			"uid":          d.Account,
			"pwd":          d.Password,
			"setSessionId": "",
			"setSig":       "",
			"setScene":     "",
			"setTocen":     "",
			"formhash":     "",
		}).Post("https://up.woozooo.com/mlogin.php")
		if err != nil {
			return nil, err
		}
		bodyStr := resp.String()
		if strings.Contains(bodyStr, "acw_sc__v2") {
			vs, err = CalcAcwScV2(bodyStr)
			if err != nil {
				return nil, err
			}
			continue
		}
		if utils.Json.Get(resp.Body(), "zt").ToInt() != 1 {
			return nil, fmt.Errorf("login err: %s", resp.Body())
		}
		d.Cookie = CookieToString(resp.Cookies())
		return resp.Cookies(), nil
	}
	return nil, errors.New("acw_sc__v2 validation error")
}

func (d *LanZou) GetAllFiles(folderID string) ([]model.Obj, error) {
	folders, err := d.GetFolders(folderID)
	if err != nil {
		return nil, err
	}
	files, err := d.GetFiles(folderID)
	if err != nil {
		return nil, err
	}
	return append(
		utils.MustSliceConvert(folders, func(folder FileOrFolder) model.Obj {
			return &folder
		}), utils.MustSliceConvert(files, func(file FileOrFolder) model.Obj {
			return &file
		})...,
	), nil
}

func (d *LanZou) GetFolders(folderID string) ([]FileOrFolder, error) {
	var resp RespText[[]FileOrFolder]
	_, err := d.doupload(func(req *resty.Request) {
		req.SetFormData(map[string]string{
			"task":      "47",
			"folder_id": folderID,
		})
	}, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Text, nil
}

func (d *LanZou) GetFiles(folderID string) ([]FileOrFolder, error) {
	files := make([]FileOrFolder, 0)
	for pg := 1; ; pg++ {
		var resp RespText[[]FileOrFolder]
		_, err := d.doupload(func(req *resty.Request) {
			req.SetFormData(map[string]string{
				"task":      "5",
				"folder_id": folderID,
				"pg":        strconv.Itoa(pg),
			})
		}, &resp)
		if err != nil {
			return nil, err
		}
		if len(resp.Text) == 0 {
			break
		}
		files = append(files, resp.Text...)
	}
	return files, nil
}

func (d *LanZou) getFolderShareUrlByID(fileID string) (*FileShare, error) {
	var resp RespInfo[FileShare]
	_, err := d.doupload(func(req *resty.Request) {
		req.SetFormData(map[string]string{
			"task":    "18",
			"file_id": fileID,
		})
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp.Info, nil
}

func (d *LanZou) getFileShareUrlByID(fileID string) (*FileShare, error) {
	var resp RespInfo[FileShare]
	_, err := d.doupload(func(req *resty.Request) {
		req.SetFormData(map[string]string{
			"task":    "22",
			"file_id": fileID,
		})
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp.Info, nil
}

var isFileReg = regexp.MustCompile(`class="fileinfo"|id="file"|文件描述`)
var isFolderReg = regexp.MustCompile(`id="infos"`)

var nameFindReg = regexp.MustCompile(`<title>(.+?) - 蓝奏云</title>|id="filenajax">(.+?)</div>|var filename = '(.+?)';|<div style="font-size.+?>([^<>].+?)</div>|<div class="filethetext".+?>([^<>]+?)</div>`)
var sizeFindReg = regexp.MustCompile(`(?i)大小\W*([0-9.]+\s*[bkm]+)`)
var timeFindReg = regexp.MustCompile(`\d+\s*[秒天分小][钟时]?前|[昨前]天|\d{4}-\d{2}-\d{2}`)
var findSubFolderReg = regexp.MustCompile(`(?i)(?:folderlink|mbxfolder).+href="/(.+?)"(?:.+filename")?>(.+?)<`)
var findDownPageParamReg = regexp.MustCompile(`<iframe.*?src="(.+?)"`)

// ========== 修改点 1：正则匹配完整 URL ==========
var findFileIDReg = regexp.MustCompile(`(https?://[^"'\s]+/ajax(?:file|m)\.php\?file=\d+)`)

func (d *LanZou) getShareUrlHtml(shareID string) (string, error) {
	var vs string
	for i := 0; i < 3; i++ {
		firstPageData, err := d.get(fmt.Sprint(d.ShareUrl, "/", shareID),
			func(req *resty.Request) {
				if vs != "" {
					req.SetCookie(&http.Cookie{
						Name:  "acw_sc__v2",
						Value: vs,
					})
				}
			})
		if err != nil {
			return "", err
		}

		firstPageDataStr := RemoveNotes(string(firstPageData))
		if strings.Contains(firstPageDataStr, "取消分享") {
			return "", ErrFileShareCancel
		}
		if strings.Contains(firstPageDataStr, "文件不存在") {
			return "", ErrFileNotExist
		}

		if strings.Contains(firstPageDataStr, "acw_sc__v2") {
			if vs, err = CalcAcwScV2(firstPageDataStr); err != nil {
				log.Errorf("lanzou: err => acw_sc__v2 validation error  ,data => %s\n", firstPageDataStr)
				return "", err
			}
			continue
		}
		return firstPageDataStr, nil
	}
	return "", errors.New("acw_sc__v2 validation error")
}

func (d *LanZou) GetFileOrFolderByShareUrl(shareID, pwd string) ([]model.Obj, error) {
	pageData, err := d.getShareUrlHtml(shareID)
	if err != nil {
		return nil, err
	}

	if !isFileReg.MatchString(pageData) {
		files, err := d.getFolderByShareUrl(pwd, pageData)
		if err != nil {
			return nil, err
		}
		return utils.MustSliceConvert(files, func(file FileOrFolderByShareUrl) model.Obj {
			return &file
		}), nil
	} else {
		file, err := d.getFilesByShareUrl(shareID, pwd, pageData)
		if err != nil {
			return nil, err
		}
		return []model.Obj{file}, nil
	}
}

func (d *LanZou) GetFilesByShareUrl(shareID, pwd string) (file *FileOrFolderByShareUrl, err error) {
	pageData, err := d.getShareUrlHtml(shareID)
	if err != nil {
		return nil, err
	}
	return d.getFilesByShareUrl(shareID, pwd, pageData)
}

func (d *LanZou) getFilesByShareUrl(shareID, pwd string, sharePageData string) (*FileOrFolderByShareUrl, error) {
	var (
		param       map[string]string
		downloadUrl string
		baseUrl     string
		file        FileOrFolderByShareUrl
	)

	sharePageData = RemoveNotes(sharePageData)
	sharePageData = RemoveJSComment(sharePageData)

	if strings.Contains(sharePageData, "pwdload") || strings.Contains(sharePageData, "passwddiv") {
		sharePageData, err := getJSFunctionByName(sharePageData, "down_p")
		if err != nil {
			return nil, err
		}
		param, err := htmlJsonToMap(sharePageData)
		if err != nil {
			return nil, err
		}
		param["p"] = pwd

		// ========== 修改点 2：直接用完整 URL ==========
		matches := findFileIDReg.FindStringSubmatch(sharePageData)
		if len(matches) < 2 {
			return nil, fmt.Errorf("not find file id")
		}
		ajaxUrl := matches[1]

		var resp FileShareInfoAndUrlResp[string]
		_, err = d.post(ajaxUrl, func(req *resty.Request) { req.SetFormData(param) }, &resp)
		if err != nil {
			return nil, err
		}
		file.NameAll = resp.Inf
		file.Pwd = pwd
		baseUrl = resp.GetBaseUrl()
		downloadUrl = resp.GetDownloadUrl()
	} else {
		urlpaths := findDownPageParamReg.FindStringSubmatch(sharePageData)
		if len(urlpaths) != 2 {
			log.Errorf("lanzou: err => not find file page param ,data => %s\n", sharePageData)
			return nil, fmt.Errorf("not find file page param")
		}
		data, err := d.get(fmt.Sprint(d.ShareUrl, urlpaths[1]), nil)
		if err != nil {
			return nil, err
		}
		nextPageData := RemoveNotes(string(data))
		param, err = htmlJsonToMap(nextPageData)
		if err != nil {
			return nil, err
		}

		// ========== 修改点 3：直接用完整 URL ==========
		matches := findFileIDReg.FindStringSubmatch(nextPageData)
		if len(matches) < 2 {
			return nil, fmt.Errorf("not find file id")
		}
		ajaxUrl := matches[1]

		var resp FileShareInfoAndUrlResp[int]
		_, err = d.post(ajaxUrl, func(req *resty.Request) { req.SetFormData(param) }, &resp)
		if err != nil {
			return nil, err
		}
		baseUrl = resp.GetBaseUrl()
		downloadUrl = resp.GetDownloadUrl()

		names := nameFindReg.FindStringSubmatch(sharePageData)
		if len(names) > 1 {
			for _, name := range names[1:] {
				if name != "" {
					file.NameAll = name
					break
				}
			}
		}
	}

	sizes := sizeFindReg.FindStringSubmatch(sharePageData)
	if len(sizes) == 2 {
		file.Size = sizes[1]
	}
	file.ID = shareID
	file.Time = timeFindReg.FindString(sharePageData)

	var (
		res *resty.Response
		err error
	)
	var vs string
	var bodyStr string
	for i := 0; i < 3; i++ {
		res, err = base.NoRedirectClient.R().SetHeaders(map[string]string{
			"accept-language": "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6",
			"Referer":         baseUrl,
		}).SetDoNotParseResponse(true).
			SetCookie(&http.Cookie{
				Name:  "acw_sc__v2",
				Value: vs,
			}).SetHeader("cookie", "down_ip=1").Get(downloadUrl)
		if err != nil {
			return nil, err
		}

		if res.StatusCode() == 302 {
			if res.RawBody() != nil {
				res.RawBody().Close()
			}
			break
		}
		bodyBytes, err := io.ReadAll(res.RawBody())
		if res.RawBody() != nil {
			res.RawBody().Close()
		}
		if err != nil {
			return nil, fmt.Errorf("读取响应体失败: %w", err)
		}
		bodyStr = string(bodyBytes)
		if strings.Contains(bodyStr, "acw_sc__v2") {
			if vs, err = CalcAcwScV2(bodyStr); err != nil {
				log.Errorf("lanzou: err => acw_sc__v2 validation error  ,data => %s\n", bodyStr)
				return nil, err
			}
			continue
		}
		break
	}

	if err != nil {
		return nil, err
	}

	file.Url = res.Header().Get("location")

	if res.StatusCode() != 302 {
		param, err = htmlJsonToMap(bodyStr)
		if err != nil {
			return nil, err
		}
		param["el"] = "2"
		time.Sleep(time.Second * 2)

		var data []byte
		for i := 0; i < 3; i++ {
			data, err = d.post(fmt.Sprint(baseUrl, "/ajax.php"), func(req *resty.Request) {
				req.SetFormData(param)
				req.SetHeader("cookie", "down_ip=1")
				if vs != "" {
					req.SetCookie(&http.Cookie{
						Name:  "acw_sc__v2",
						Value: vs,
					})
				}
			}, nil)
			if err != nil {
				return nil, err
			}
			ajaxBodyStr := string(data)
			if strings.Contains(ajaxBodyStr, "acw_sc__v2") {
				if vs, err = CalcAcwScV2(ajaxBodyStr); err != nil {
					log.Errorf("lanzou: err => acw_sc__v2 validation error  ,data => %s\n", ajaxBodyStr)
					return nil, err
				}
				time.Sleep(time.Second * 2)
				continue
			}
			break
		}
		if err != nil {
			return nil, err
		}
		file.Url = utils.Json.Get(data, "url").ToString()
	}
	return &file, nil
}

func (d *LanZou) GetFolderByShareUrl(shareID, pwd string) ([]FileOrFolderByShareUrl, error) {
	pageData, err := d.getShareUrlHtml(shareID)
	if err != nil {
		return nil, err
	}
	return d.getFolderByShareUrl(pwd, pageData)
}

func (d *LanZou) getFolderByShareUrl(pwd string, sharePageData string) ([]FileOrFolderByShareUrl, error) {
	from, err := htmlJsonToMap(sharePageData)
	if err != nil {
		return nil, err
	}

	files := make([]FileOrFolderByShareUrl, 0)
	folders := findSubFolderReg.FindAllStringSubmatch(sharePageData, -1)
	for _, folder := range folders {
		if len(folder) == 3 {
			files = append(files, FileOrFolderByShareUrl{
				ID:       folder[1],
				NameAll:  folder[2],
				IsFolder: true,
			})
		}
	}

	from["pwd"] = pwd
	for page := 1; ; page++ {
		from["pg"] = strconv.Itoa(page)
		var resp FileOrFolderByShareUrlResp
		_, err := d.post(d.ShareUrl+"/filemoreajax.php", func(req *resty.Request) { req.SetFormData(from) }, &resp)
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(resp.Text); i++ {
			resp.Text[i].Pwd = pwd
		}
		if len(resp.Text) == 0 {
			break
		}
		files = append(files, resp.Text...)
		time.Sleep(time.Second)
	}
	return files, nil
}

func (d *LanZou) getFileRealInfo(downURL string) (*int64, *time.Time) {
	res, _ := base.RestyClient.R().Head(downURL)
	if res == nil {
		return nil, nil
	}
	time, _ := http.ParseTime(res.Header().Get("Last-Modified"))
	size, _ := strconv.ParseInt(res.Header().Get("Content-Length"), 10, 64)
	return &size, &time
}

func (d *LanZou) getVeiAndUid() (vei string, uid string, err error) {
	var resp []byte
	resp, err = d.get("https://pc.woozooo.com/mydisk.php", func(req *resty.Request) {
		req.SetQueryParams(map[string]string{
			"item":   "files",
			"action": "index",
		})
	})
	if err != nil {
		return
	}
	uids := regexp.MustCompile(`uid=([^'"&;]+)`).FindStringSubmatch(string(resp))
	if len(uids) < 2 {
		err = fmt.Errorf("uid variable not find")
		return
	}
	uid = uids[1]

	html := RemoveNotes(string(resp))
	data, err := htmlJsonToMap(html)
	if err != nil {
		return
	}
	vei = data["vei"]

	return
}