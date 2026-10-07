package _139

import (
	"encoding/xml"
)

const (
	MetaPersonal    string = "personal"
	MetaFamily      string = "family"
	MetaGroup       string = "group"
	MetaPersonalNew string = "personal_new"
	MetaShare       string = "share"
)

type BaseResp struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Catalog struct {
	CatalogID   string `json:"catalogID"`
	CatalogName string `json:"catalogName"`
	CreateTime string `json:"createTime"`
	UpdateTime string `json:"updateTime"`
}

type Content struct {
	ContentID   string `json:"contentID"`
	ContentName string `json:"contentName"`
	ContentSize int64 `json:"contentSize"`
	CreateTime string `json:"createTime"`
	UpdateTime string `json:"updateTime"`
	ThumbnailURL string `json:"thumbnailURL"`
	Digest string `json:"digest"`
}

type GetDiskResp struct {
	BaseResp
	Data struct {
		Result struct {
			ResultCode string      `json:"resultCode"`
			ResultDesc interface{} `json:"resultDesc"`
		} `json:"result"`
		GetDiskResult struct {
			ParentCatalogID string    `json:"parentCatalogID"`
			NodeCount       int       `json:"nodeCount"`
			CatalogList     []Catalog `json:"catalogList"`
			ContentList     []Content `json:"contentList"`
			IsCompleted     int       `json:"isCompleted"`
		} `json:"getDiskResult"`
	} `json:"data"`
}

type UploadResp struct {
	BaseResp
	Data struct {
		Result struct {
			ResultCode string      `json:"resultCode"`
			ResultDesc interface{} `json:"resultDesc"`
		} `json:"result"`
		UploadResult struct {
			UploadTaskID     string `json:"uploadTaskID"`
			RedirectionURL   string `json:"redirectionUrl"`
			NewContentIDList []struct {
				ContentID     string `json:"contentID"`
				ContentName   string `json:"contentName"`
				IsNeedUpload  string `json:"isNeedUpload"`
				FileEtag      int64  `json:"fileEtag"`
				FileVersion   int64  `json:"fileVersion"`
				OverridenFlag int    `json:"overridenFlag"`
			} `json:"newContentIDList"`
			CatalogIDList interface{} `json:"catalogIDList"`
			IsSlice       interface{} `json:"isSlice"`
		} `json:"uploadResult"`
	} `json:"data"`
}

type InterLayerUploadResult struct {
	XMLName    xml.Name `xml:"result"`
	Text       string   `xml:",chardata"`
	ResultCode int      `xml:"resultCode"`
	Msg        string   `xml:"msg"`
}

type CloudContent struct {
	ContentID string `json:"contentID"`
	ContentName string `json:"contentName"`
	ContentSize int64 `json:"contentSize"`
	CreateTime string `json:"createTime"`
	LastUpdateTime string `json:"lastUpdateTime"`
	ThumbnailURL   string `json:"thumbnailURL"`
}

type CloudCatalog struct {
	CatalogID   string `json:"catalogID"`
	CatalogName string `json:"catalogName"`
	CreateTime     string `json:"createTime"`
	LastUpdateTime string `json:"lastUpdateTime"`
}

type QueryContentListResp struct {
	BaseResp
	Data struct {
		Result struct {
			ResultCode string `json:"resultCode"`
			ResultDesc string `json:"resultDesc"`
		} `json:"result"`
		Path             string         `json:"path"`
		CloudContentList []CloudContent `json:"cloudContentList"`
		CloudCatalogList []CloudCatalog `json:"cloudCatalogList"`
		TotalCount       int            `json:"totalCount"`
		RecallContent    interface{}    `json:"recallContent"`
	} `json:"data"`
}

type QueryGroupContentListResp struct {
	BaseResp
	Data struct {
		Result struct {
			ResultCode string `json:"resultCode"`
			ResultDesc string `json:"resultDesc"`
		} `json:"result"`
		GetGroupContentResult struct {
			ParentCatalogID string `json:"parentCatalogID"`
			CatalogList     []struct {
				Catalog
				Path string `json:"path"`
			} `json:"catalogList"`
			ContentList []Content `json:"contentList"`
			NodeCount   int       `json:"nodeCount"`
			CtlgCnt     int       `json:"ctlgCnt"`
			ContCnt     int       `json:"contCnt"`
		} `json:"getGroupContentResult"`
	} `json:"data"`
}

type ParallelHashCtx struct {
	PartOffset int64 `json:"partOffset"`
}

type PartInfo struct {
	PartNumber      int64           `json:"partNumber"`
	PartSize        int64           `json:"partSize"`
	ParallelHashCtx ParallelHashCtx `json:"parallelHashCtx"`
}

type PersonalThumbnail struct {
	Style string `json:"style"`
	Url   string `json:"url"`
}

type PersonalFileItem struct {
	FileId     string              `json:"fileId"`
	Name       string              `json:"name"`
	Size       int64               `json:"size"`
	Type       string              `json:"type"`
	CreatedAt  string              `json:"createdAt"`
	UpdatedAt  string              `json:"updatedAt"`
	Thumbnails []PersonalThumbnail `json:"thumbnailUrls"`
}

type PersonalListResp struct {
	BaseResp
	Data struct {
		Items          []PersonalFileItem `json:"items"`
		NextPageCursor string             `json:"nextPageCursor"`
	}
}

type PersonalPartInfo struct {
	PartNumber int    `json:"partNumber"`
	UploadUrl  string `json:"uploadUrl"`
}

type PersonalUploadResp struct {
	BaseResp
	Data struct {
		FileId      string             `json:"fileId"`
		FileName    string             `json:"fileName"`
		PartInfos   []PersonalPartInfo `json:"partInfos"`
		Exist       bool               `json:"exist"`
		RapidUpload bool               `json:"rapidUpload"`
		UploadId    string             `json:"uploadId"`
	}
}

type PersonalUploadUrlResp struct {
	BaseResp
	Data struct {
		FileId    string             `json:"fileId"`
		UploadId  string             `json:"uploadId"`
		PartInfos []PersonalPartInfo `json:"partInfos"`
	}
}

type ShareCatalog struct {
	CaID   string `json:"caId"`
	CaName string `json:"caName"`
	UdTime string `json:"udTime"`
}

type ShareContent struct {
	CoID        string `json:"coId"`
	CoName      string `json:"coName"`
	CoSize      int64  `json:"coSize"`
	CoType      int    `json:"coType"`
	UdTime      string `json:"udTime"`
	CoPath      string `json:"coPath"`
	PresentURL  string `json:"presentURL"`
	DownloadURL string `json:"downloadURL"`
}

type ShareListResp struct {
	BaseResp
	Data struct {
		LKName string         `json:"lkName"`
		Passwd string         `json:"password"`
		CaLst  []ShareCatalog `json:"caLst"`
		CoLst  []ShareContent `json:"coLst"`
	} `json:"data"`
}

type ShareContentInfo struct {
	PresentURL  string `json:"presentURL"`
	DownloadURL string `json:"cdnDownLoadUrl"`
}

type ShareDownloadResp struct {
	BaseResp
	Data struct {
		DownloadURL string `json:"downloadURL"`
		RedrURL     string `json:"redrUrl"`
		ExtInfo     struct {
			CDNDownloadURL string `json:"cdnDownloadUrl"`
		} `json:"extInfo"`
	} `json:"data"`
}

type ShareContentInfoResp struct {
	BaseResp
	Data struct {
		ContentInfo ShareContentInfo `json:"contentInfo"`
	} `json:"data"`
}

type QueryRoutePolicyResp struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RoutePolicyList []struct {
			SiteID      string `json:"siteID"`
			SiteCode    string `json:"siteCode"`
			ModName     string `json:"modName"`
			HttpUrl     string `json:"httpUrl"`
			HttpsUrl    string `json:"httpsUrl"`
			EnvID       string `json:"envID"`
			ExtInfo     string `json:"extInfo"`
			HashName    string `json:"hashName"`
			ModAddrType int    `json:"modAddrType"`
		} `json:"routePolicyList"`
	} `json:"data"`
}

type RefreshTokenResp struct {
	XMLName     xml.Name `xml:"root"`
	Return      string   `xml:"return"`
	Token       string   `xml:"token"`
	Expiretime  int32    `xml:"expiretime"`
	AccessToken string   `xml:"accessToken"`
	Desc        string   `xml:"desc"`
}

type DiskQuotaDetail struct {
	BaseResp
	Data struct {
		FreeDiskSize int64 `json:"freeDiskSize"`
		DiskSize     int64 `json:"diskSize"`
	} `json:"data"`
}

type AndAlbumUploadResp struct {
	Result struct {
		ResultCode string `json:"resultCode"`
		ResultDesc string `json:"resultDesc"`
	} `json:"result"`
	UploadResult struct {
		UploadTaskID     string `json:"uploadTaskID"`
		RedirectionURL   string `json:"redirectionUrl"`
		NewContentIDList []struct {
			ContentID   string `json:"contentID"`
			ContentName string `json:"contentName"`
		} `json:"newContentIDList"`
	} `json:"uploadResult"`
}

type ModifyCloudDocV2Req struct {
	CatalogType       int    `json:"catalogType"`
	CloudID           string `json:"cloudID"`
	CommonAccountInfo struct {
		Account     string `json:"account"`
		AccountType string `json:"accountType"`
	} `json:"commonAccountInfo"`
	DocLibName   string `json:"docLibName"`
	DocLibraryID string `json:"docLibraryID"`
	Path         string `json:"path"`
}

type ModifyCloudDocV2Resp struct {
	Result struct {
		ResultCode string `json:"resultCode"`
		ResultDesc string `json:"resultDesc"`
	} `json:"result"`
}

type CreateBatchOprTaskReq struct {
	CatalogList       []string `json:"catalogList"`
	CommonAccountInfo struct {
		Account     string `json:"account"`
		AccountType string `json:"accountType"`
	} `json:"commonAccountInfo"`
	ContentList       []string `json:"contentList"`
	DestCatalogID     string   `json:"destCatalogID"`
	DestGroupID       string   `json:"destGroupID"`
	DestPath          string   `json:"destPath"`
	DestType          int      `json:"destType"`
	SourceCatalogType int      `json:"sourceCatalogType"`
	SourceCloudID     string   `json:"sourceCloudID"`
	SourceType        int      `json:"sourceType"`
	TaskType          int      `json:"taskType"`
}

type CreateBatchOprTaskResp struct {
	Result struct {
		ResultCode string `json:"resultCode"`
		ResultDesc string `json:"resultDesc"`
	} `json:"result"`
	TaskID string `json:"taskID"`
}

// ============================================================
// ★ 新增：videoPreview/getPreviewInfo 响应结构
// ============================================================

type VideoPreviewResp struct {
	BaseResp
	Data struct {
		FileID string `json:"fileId"`
		Meta   struct {
			Duration     string  `json:"duration"`
			Width        int     `json:"width"`
			Height       int     `json:"height"`
			TakenAt      *string `json:"takenAt"`
			LivePhoto    bool    `json:"livePhoto"`
			Make         *string `json:"make"`
			Model        *string `json:"model"`
			DolbyVision  bool    `json:"dolbyVision"`
			MultiChannel bool    `json:"multiChannel"`
		} `json:"meta"`
		PreviewInfo struct {
			Status string `json:"status"`
			URL    string `json:"url"`
		} `json:"previewInfo"`
		ErrorCode interface{} `json:"errorCode"`
		Message   interface{} `json:"message"`
	} `json:"data"`
}