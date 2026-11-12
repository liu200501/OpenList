package crypt

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	FileNameEnc string `json:"filename_encryption" type:"select" required:"true" options:"off,standard,obfuscate" default:"off"`
	DirNameEnc  string `json:"directory_name_encryption" type:"select" required:"true" options:"false,true" default:"false"`
	RemotePath  string `json:"remote_path" required:"true" help:"This is where the encrypted data stores"`

	Password         string `json:"password" required:"true" confidential:"true" help:"the main password"`
	Salt             string `json:"salt" confidential:"true"  help:"If you don't know what is salt, treat it as a second password. Optional but recommended"`
	EncryptedSuffix  string `json:"encrypted_suffix" required:"true" default:".bin" help:"for advanced user only! encrypted files will have this suffix"`
	FileNameEncoding string `json:"filename_encoding" type:"select" required:"true" options:"base64,base32,base32768" default:"base64" help:"for advanced user only!"`

	Thumbnail bool `json:"thumbnail" required:"true" default:"false" help:"enable thumbnail which pre-generated under .thumbnails folder"`

	// 用「禁用」而不是「启用」：老配置的 addition JSON 里没有这个 key，
	// 反序列化后为 false，正好保持原有的「支持暂停」行为。
	DisableResumable bool `json:"disable_resumable_upload" default:"false" help:"turn off pause/resume for uploads: no ciphertext is pre-written to the temp dir, and an interrupted upload has to restart from the beginning"`

	ShowHidden bool `json:"show_hidden"  default:"true" required:"false" help:"show hidden directories and files"`

	// 后台全量扫描，生成完整的「明文 ↔ 密文」索引（不必逐层浏览）。
	// 打开后每次存储加载会在后台遍历整棵目录树；大容量存储会持续较久。
	NameMapScan bool `json:"name_map_scan" default:"true" required:"false" help:"build a full plaintext/ciphertext name index in the background on startup (slow on large drives)"`
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
