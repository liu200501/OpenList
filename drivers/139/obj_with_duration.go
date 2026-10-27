package _139


import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// objWithDuration 包装 model.Obj，附加时长字段
// 实现 model.Obj 的所有方法，并额外实现 GetDuration()
type objWithDuration struct {
	model.Obj
	duration float64
}

func (o *objWithDuration) GetDuration() float64 {
	return o.duration
}

// 为了不丢失原有的 ObjThumb 能力，把关键接口也转发一遍
func (o *objWithDuration) GetName() string          { return o.Obj.GetName() }
func (o *objWithDuration) GetSize() int64           { return o.Obj.GetSize() }
func (o *objWithDuration) IsDir() bool              { return o.Obj.IsDir() }
func (o *objWithDuration) ModTime() time.Time       { return o.Obj.ModTime() }
func (o *objWithDuration) CreateTime() time.Time    { return o.Obj.CreateTime() }
func (o *objWithDuration) GetHash() utils.HashInfo  { return o.Obj.GetHash() }
func (o *objWithDuration) GetID() string            { return o.Obj.GetID() }
func (o *objWithDuration) GetPath() string          { return o.Obj.GetPath() }

// attachDuration 把时长附加到 obj 上。
// 如果 obj 已经是 objWithDuration，直接更新；否则包一层。
func attachDuration(obj model.Obj, dur float64) model.Obj {
	if w, ok := obj.(*objWithDuration); ok {
		w.duration = dur
		return w
	}
	return &objWithDuration{Obj: obj, duration: dur}
}

// GetDurationFromObj 供 fsread.go 通过接口读取时长
func GetDurationFromObj(obj model.Obj) float64 {
	if p, ok := obj.(interface{ GetDuration() float64 }); ok {
		return p.GetDuration()
	}
	return 0
}