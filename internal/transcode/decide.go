package transcode

import (
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
)

// DecideRequest 决策器输入
type DecideRequest struct {
	Path     string // 文件完整路径，用于"仅指定文件夹转码"范围判断
	FileName string
	FileSize int64       // 字节
	Probe    SourceProbe // 可选；为空则只用大小+后缀判断
}

// DecideResult 决策结果
type DecideResult struct {
	NeedTranscode bool
	Reason        string
}

// Decide 根据全局设置 + 文件路径判断是否需要走转码。
// 视频/音频是否单独转码由播放端在 BuildProfile 时决定（齿轮开关），
// 这里只判断"这个文件是否允许进入转码流程"。
func Decide(r DecideRequest) DecideResult {
	if !setting.GetBool(conf.TranscodeEnabled) {
		return DecideResult{false, "transcode disabled"}
	}
	// 0. 转码范围：仅指定文件夹时，路径不在白名单内一律不转码。
	if !pathInTranscodeScope(r.Path) {
		return DecideResult{false, "path not in transcode scope"}
	}
	// 1. 大小阈值
	minGB := setting.GetInt(conf.TranscodeMinSizeGB, 5)
	if minGB > 0 {
		minBytes := int64(minGB) * 1024 * 1024 * 1024
		if r.FileSize > 0 && r.FileSize < minBytes {
			return DecideResult{false, "size below threshold"}
		}
	}
	// 2. 后缀过滤
	exts := splitCSV(setting.GetStr(conf.TranscodeSourceExtensions))
	if len(exts) > 0 {
		ext := strings.ToLower(strings.TrimPrefix(getExt(r.FileName), "."))
		if !containsFold(exts, ext) {
			return DecideResult{false, "extension not in list"}
		}
	}
	// 3. 编码过滤（仅当 probe 提供时）
	codecs := splitCSV(setting.GetStr(conf.TranscodeSourceCodecs))
	if r.Probe.VideoCodec != "" && len(codecs) > 0 {
		if !containsFold(codecs, r.Probe.VideoCodec) {
			return DecideResult{false, "codec not in list"}
		}
	}
	// 4. 码率过滤（仅当 probe 提供时）
	minBR := setting.GetInt(conf.TranscodeMinBitrateMbps, 0)
	if minBR > 0 && r.Probe.VideoBitrate > 0 {
		if r.Probe.VideoBitrate < int64(minBR)*1_000_000 {
			return DecideResult{false, "bitrate below threshold"}
		}
	}
	return DecideResult{true, "ok"}
}

// pathInTranscodeScope 判断文件路径是否落在允许转码的范围内。
// scope=all（默认）或空 → 全部允许；scope=paths → 仅前缀匹配 scope_paths 中的目录。
func pathInTranscodeScope(path string) bool {
	scope := strings.ToLower(strings.TrimSpace(setting.GetStr(conf.TranscodeScope, "all")))
	if scope != "paths" {
		return true
	}
	raw := setting.GetStr(conf.TranscodeScopePaths)
	// 同时支持换行与逗号分隔
	raw = strings.ReplaceAll(raw, ",", "\n")
	for _, line := range strings.Split(raw, "\n") {
		dir := strings.TrimSpace(line)
		if dir == "" {
			continue
		}
		// 统一去掉首尾斜杠，做路径段前缀匹配，避免 /mov 误匹配 /movies。
		d := strings.Trim(dir, "/")
		p := strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "/")
		if d == "" || p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// PathAllowedForTranscode 供 handler 在探测前复用的公开判断（同一份逻辑）。
func PathAllowedForTranscode(path string) bool { return pathInTranscodeScope(path) }

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsFold(arr []string, v string) bool {
	for _, a := range arr {
		if strings.EqualFold(a, v) {
			return true
		}
	}
	return false
}

func getExt(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return name[i:]
}

// BuildProfile 根据当前全局设置构建默认 Profile。
// videoOn/audioOn 来自播放端"齿轮开关"：关闭的那一路使用 copy（不重编码）。
func BuildProfile(videoOn, audioOn bool) Profile {
	res := setting.GetStr(conf.TranscodeOutputResolution)
	scale := ""
	name := "default"
	switch res {
	case "source":
		scale = ""
		name = "source"
	case "3840x2160":
		scale = "3840:-2"
		name = "2160p"
	case "2560x1440":
		scale = "2560:-2"
		name = "1440p"
	case "1920x1080":
		scale = "1920:-2"
		name = "1080p"
	case "1280x720":
		scale = "1280:-2"
		name = "720p"
	case "854x480":
		scale = "854:-2"
		name = "480p"
	}
	// 播放端齿轮开关独立生效：关掉的那一路使用 copy（保留源编码、不缩放/不重编码）。
	videoCodec := setting.GetStr(conf.TranscodeOutputCodec)
	videoBitrate := setting.GetStr(conf.TranscodeOutputBitrate)
	audioCodec := setting.GetStr(conf.TranscodeOutputAudioCodec)
	if !videoOn {
		videoCodec = "copy"
		videoBitrate = ""
		scale = ""
	}
	if !audioOn {
		audioCodec = "copy"
	}
	return Profile{
		Name:         name,
		Kind:         "muxed",
		VideoCodec:   videoCodec,
		VideoBitrate: videoBitrate,
		Scale:        scale,
		AudioCodec:   audioCodec,
		AudioBitrate: setting.GetStr(conf.TranscodeOutputAudioBitrate),
		HWAccel:      setting.GetStr(conf.TranscodeHWAccel),
	}
}

// BuildRenditions 根据探测到的轨道生成转码输出集合。
//
//   - 单视频轨 + 单音轨：保持历史行为，输出一个"音视频混流"的 HLS 档位（Kind=muxed）。
//   - 多轨（MKV 常见）：输出
//     v0..vN  每个视频轨一条纯视频 rendition（只做分辨率缩放 + 码率控制，不带音频）
//     a0..aM  每个音轨一条纯音频 rendition（统一重编码为 AAC 立体声 48kHz）
//     master playlist 用 EXT-X-MEDIA 音频组把它们组装起来，播放器即可切换音轨；
//     切换视频轨等价于切换 HLS 的 variant（level）。
func BuildRenditions(probe SourceProbe, videoOn, audioOn bool) []Profile {
	return buildRenditions(probe, BuildProfile(videoOn, audioOn))
}

// buildRenditions 是 BuildRenditions 的纯函数版本，便于单测（不依赖设置库）。
func buildRenditions(probe SourceProbe, base Profile) []Profile {
	vTracks := probe.VideoTracks
	aTracks := probe.AudioTracks
	if len(vTracks) <= 1 && len(aTracks) <= 1 {
		return []Profile{base}
	}
	var out []Profile
	for i := range vTracks {
		p := base
		p.Name = "v" + strconv.Itoa(i)
		p.Kind = "video"
		p.TrackIndex = i
		p.AudioCodec = ""
		p.AudioBitrate = ""
		p.Language = vTracks[i].Language
		p.Title = vTracks[i].Title
		out = append(out, p)
	}
	for i := range aTracks {
		p := base
		p.Name = "a" + strconv.Itoa(i)
		p.Kind = "audio"
		p.TrackIndex = i
		p.VideoCodec = ""
		p.VideoBitrate = ""
		p.Scale = ""
		// 音频转码开关关闭时保留源编码（copy）；开启时统一转 AAC，
		// 因为浏览器 MSE 不支持 AC3/DTS/EAC3/TrueHD 等原始音轨。
		if p.AudioCodec != "copy" {
			p.AudioCodec = "aac"
			p.AudioBitrate = audioBitrateFor(aTracks[i], base.AudioBitrate)
		}
		p.Channels = aTracks[i].Channels
		p.Language = aTracks[i].Language
		p.Title = aTracks[i].Title
		out = append(out, p)
	}
	return out
}

// audioBitrateFor 按声道数给出合理的 AAC 码率；管理员显式配置了码率时优先用配置值。
func audioBitrateFor(t TrackInfo, configured string) string {
	if configured != "" && configured != "160k" {
		return configured
	}
	switch {
	case t.Channels >= 6:
		return "320k"
	case t.Channels == 2:
		return "160k"
	case t.Channels == 1:
		return "96k"
	}
	if configured != "" {
		return configured
	}
	return "160k"
}

// BuildOutputSpec 根据当前全局设置构建输出规格
func BuildOutputSpec() OutputSpec {
	return OutputSpec{
		Format:          OutputFormat(setting.GetStr(conf.TranscodeOutputFormat)),
		SegmentDuration: setting.GetInt(conf.TranscodeSegmentDuration, 6),
	}
}
