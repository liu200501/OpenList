package crypt

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	FileNameEnc string `json:"filename_encryption" type:"select" required:"true" options:"off,standard,obfuscate" default:"off"`
	DirNameEnc  string `json:"directory_name_encryption" type:"select" required:"true" options:"false,true" default:"false"`
	RemotePath  string `json:"remote_path" required:"true" help:"存放加密数据的远端目录"`

	Password         string `json:"password" required:"true" confidential:"true" help:"主密码"`
	Salt             string `json:"salt" confidential:"true"  help:"如果不了解盐值，可把它当作第二个密码。可选，但建议使用"`
	EncryptedSuffix  string `json:"encrypted_suffix" required:"true" default:".bin" help:"仅供高级用户使用！加密文件将使用此后缀"`
	FileNameEncoding string `json:"filename_encoding" type:"select" required:"true" options:"base64,base32,base32768" default:"base64" help:"仅供高级用户使用！"`

	Thumbnail bool `json:"thumbnail" required:"true" default:"false" help:"启用缩略图（缩略图需预先生成在 .thumbnails 目录下）"`

	// 用「禁用」而不是「启用」：老配置的 addition JSON 里没有这个 key，
	// 反序列化后为 false，正好保持原有的「支持暂停」行为。
	DisableResumable bool `json:"disable_resumable_upload" default:"false" help:"关闭上传的暂停/继续：不再把密文预先写入临时目录，上传中断后必须从头开始"`

	ShowHidden bool `json:"show_hidden"  default:"true" required:"false" help:"显示隐藏的目录与文件"`

	// 后台全量扫描，生成完整的「明文 ↔ 密文」索引（不必逐层浏览）。
	// 打开后每次存储加载会在后台遍历整棵目录树；大容量存储会持续较久。
	NameMapScan bool `json:"name_map_scan" default:"true" required:"false" help:"启动时在后台建立完整的明文/密文名称对照索引（大容量存储会较慢）"`
}

var config = driver.Config{
	Name:        "Crypt",
	LocalSort:   true,
	OnlyProxy:   true,
	NoCache:     true,
	DefaultRoot: "/",
	NoLinkURL:   true,
	CheckStatus: true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Crypt{}
	})
}
