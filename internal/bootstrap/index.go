package bootstrap

import (
	"github.com/OpenListTeam/OpenList/v4/internal/search"
	log "github.com/sirupsen/logrus"
)

func InitIndex() {
	progress, err := search.Progress()
	if err != nil {
		log.Errorf("init index error: %+v", err)
		return
	}
	// 不要把“未完成/从未构建”的进度伪造成完成状态。
	// 以前这里无条件把 IsDone 置为 true，导致 index_progress 显示
	// {"is_done":true,"obj_count":0,"last_done_time":null}，增量更新
	// 误以为索引已经建好，搜索却一直搜不到文件。
	// 只有当确实有一次成功构建（有完成时间且没有错误）时才认为是完成。
	done := progress.IsDone && progress.LastDoneTime != nil && progress.Error == ""
	if progress.IsDone && !done {
		progress.IsDone = false
		search.WriteProgress(progress)
		log.Warnf("index progress was marked done without a successful build; reset to unfinished")
	}
}
