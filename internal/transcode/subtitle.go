package transcode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// 字幕抽取：把内嵌文本字幕轨转成 WebVTT，按需生成并缓存到 job 目录下。
//
// 只处理文本字幕（SRT/ASS/SSA/MOV_TEXT 等），PGS/DVD 这类图形字幕无法转文本，
// 属于「需要烧录」的范畴，不在直接字幕轨能力内。

var subtitleLocks sync.Map // path -> *sync.Mutex

// SubtitlePath 返回某个字幕轨对应的缓存文件路径。
func SubtitlePath(dir string, ordinal int) string {
	return filepath.Join(dir, fmt.Sprintf("sub-%d.vtt", ordinal))
}

// EnsureSubtitle 确保第 ordinal 条字幕轨的 WebVTT 文件已生成，返回其路径。
func EnsureSubtitle(job *Job, ordinal int, dir string) (string, error) {
	if ordinal < 0 || ordinal >= len(job.Probe.SubtitleTracks) {
		return "", fmt.Errorf("subtitle track %d not found", ordinal)
	}
	track := job.Probe.SubtitleTracks[ordinal]
	if !track.Textual {
		return "", fmt.Errorf("subtitle track %d is bitmap (codec=%s), cannot convert to webvtt", ordinal, track.Codec)
	}
	out := SubtitlePath(dir, ordinal)
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		return out, nil
	}

	muAny, _ := subtitleLocks.LoadOrStore(out, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	// 双检：等锁期间可能已经被另一个请求生成
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		return out, nil
	}

	ffmpegPath := setting.GetStr(conf.TranscodeFFmpegPath, "ffmpeg")
	tmp := out + ".tmp"
	args := []string{
		"-y", "-loglevel", "error",
		"-protocol_whitelist", ProtocolWhitelistFor(job.SourceURL),
		"-i", job.SourceURL,
		"-map", fmt.Sprintf("0:s:%d", ordinal),
		"-c:s", "webvtt",
		"-f", "webvtt",
		tmp,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr limitedBuffer
	stderr.maxSize = 32 * 1024
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("extract subtitle failed: %v, stderr=%s", err, truncate(stderr.String(), 1024))
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("subtitle output empty")
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	utils.Log.Infof("[transcode] subtitle extracted: job=%s track=%d -> %s", job.ID, ordinal, out)
	return out, nil
}

// ParseSubtitleOrdinal 解析 "3.vtt" / "3" 这类字幕文件名。
func ParseSubtitleOrdinal(name string) (int, bool) {
	if filepath.Ext(name) != ".vtt" {
		return 0, false
	}
	n, err := strconv.Atoi(name[:len(name)-len(".vtt")])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
