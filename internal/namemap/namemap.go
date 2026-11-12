package namemap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
)

// Dir 返回对照页的存放目录：与数据库同级的 cryptmap/。
// 放在数据目录而不是临时目录，避免重启时被清理。
func Dir() string {
	base := ""
	if conf.Conf != nil && conf.Conf.Database.DBFile != "" {
		base = filepath.Dir(conf.Conf.Database.DBFile)
	}
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "cryptmap")
}

func HTMLPath(id uint) string {
	return filepath.Join(Dir(), fmt.Sprintf("%d.html", id))
}

func JSONPath(id uint) string {
	return filepath.Join(Dir(), fmt.Sprintf("%d.json", id))
}

func EnsureDir() error {
	return os.MkdirAll(Dir(), 0o755)
}

type stateFile struct {
	Mount string `json:"mount"`
}

// ReadMount 从索引文件里读出挂载点，供路由侧定位存储实例。
func ReadMount(id uint) string {
	b, err := os.ReadFile(JSONPath(id))
	if err != nil {
		return ""
	}
	var s stateFile
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.Mount
}
