package _139

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	//Account       string `json:"account" required:"true"`
	Authorization string `json:"authorization" type:"text" help:"Authorization can be used alone. If empty, use username + password; mail_cookies is optional and will be established/updated automatically. Existing mail_cookies can also be used alone for fast login."`
	Username      string `json:"username" help:"Use together with password when Authorization is empty. mail_cookies may be left empty on the first login."`
	Password      string `json:"password" secret:"true" help:"Use together with username when Authorization is empty. mail_cookies may be left empty on the first login."`
	SmsCode       string `json:"sms_code" secret:"true" help:"Fill this only after OpenList reports that a 139 Mail SMS verification code was sent, then save the storage again."`
	MailCookies   string `json:"mail_cookies" type:"text" help:"Optional cookies from mail.10086.cn. Leave empty for a first username/password login; cookies created or updated by password/SMS login are persisted and reused as device context. Existing cookies may also be used alone for fast login."`
	driver.RootID
	Type                 string `json:"type" type:"select" options:"personal_new,family,group,personal,share" default:"personal_new"`
	LinkID               string `json:"link_id" type:"text" help:"Multiple shares are separated by commas or new lines. Use link_id#password for password-protected shares."`
	CloudID              string `json:"cloud_id"`
	UserDomainID         string `json:"user_domain_id" help:"ud_id in Cookie, fill in to show disk usage"`
	CustomUploadPartSize int64  `json:"custom_upload_part_size" type:"number" default:"0" help:"0 for auto"`
	ReportRealSize       bool   `json:"report_real_size" type:"bool" default:"true" help:"Enable to report the real file size during upload"`
	UseLargeThumbnail    bool   `json:"use_large_thumbnail" type:"bool" default:"false" help:"Enable to use large thumbnail for images"`
	UseOldStreamUpload   bool   `json:"use_old_stream_upload" type:"bool" default:"false" help:"Enable to use old stream upload method (not support rapid upload)"`
	ClientType           string `json:"client_type" type:"select" options:"web,pc" default:"web" help:"Client type reported to the personal cloud API. web (9) allows uploading files up to 50 GB; pc (13) raises the single file upload limit to 500 GB."`
		// 新增：缩略图代理
	ThumbnailProxyEnabled bool   `json:"thumbnail_proxy_enabled" type:"bool" default:"false" help:"启用缩略图代理：开启后，缩略图 URL 会替换为下方代理地址，解决 *.cmecloud.cn 直连不可达的问题"`
	ThumbnailProxyURL     string `json:"thumbnail_proxy_url" type:"string" help:"缩略图代理地址前缀。代理服务需接收 ?url=原始缩略图URL 并转发，同时保持原始 Host 头以通过 AWS 签名校验。示例：https://your-proxy.com/proxy?url="`
	// 跨账号秒转存：本存储作为目标账号，source_storage_id 指向源账号的另一个 139 存储 ID。
	// 转存时源账号负责创建分享，本账号负责订阅并转存，两边各用各自的授权。
	SourceStorageID string `json:"source_storage_id" type:"string" help:"跨账号秒转存：源账号的 139 存储 ID（数字）。留空则不启用。转存时源账号建分享、本账号订阅并转存，各用各的授权。"`
}

var config = driver.Config{
	Name:             "139Yun",
	LocalSort:        true,
	ProxyRangeOption: true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		d := &Yun139{}
		d.ProxyRange = true
		return d
	})
}