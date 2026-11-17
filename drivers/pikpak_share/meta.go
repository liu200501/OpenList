package pikpak_share

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	ShareId               string `json:"share_id" required:"true"`
	SharePwd              string `json:"share_pwd"`
	Platform              string `json:"platform" default:"web" required:"true" type:"select" options:"android,web,pc"`
	DeviceID              string `json:"device_id"  required:"false" default:""`
	UseTransCodingAddress bool   `json:"use_transcoding_address" required:"true" default:"false"`
	APIProxy              string `json:"api_proxy" default:"" help:"HTTP(S) proxy for PikPak API requests (list, link), e.g. http://127.0.0.1:10808. Leave empty to use default."`
}

var config = driver.Config{
	Name:      "PikPakShare",
	LocalSort: true,
	NoUpload:  true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &PikPakShare{}
	})
}
