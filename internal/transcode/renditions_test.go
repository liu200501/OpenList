package transcode

import (
	"strings"
	"testing"
)

func testBase() Profile {
	return Profile{
		Name:         "1080p",
		Kind:         "muxed",
		VideoCodec:   "h264",
		VideoBitrate: "4000k",
		Scale:        "1920:-2",
		AudioCodec:   "copy",
		AudioBitrate: "160k",
	}
}

func TestBuildRenditionsSingleTrack(t *testing.T) {
	probe := SourceProbe{
		VideoTracks: []TrackInfo{{Index: 0, CodecType: "video", Codec: "h264"}},
		AudioTracks: []TrackInfo{{Index: 1, CodecType: "audio", Codec: "aac", Channels: 2}},
	}
	got := buildRenditions(probe, testBase())
	if len(got) != 1 || got[0].Name != "1080p" || got[0].Kind != "muxed" {
		t.Fatalf("single track should stay muxed, got %+v", got)
	}
}

func TestBuildRenditionsMultiTrack(t *testing.T) {
	probe := SourceProbe{
		VideoTracks: []TrackInfo{
			{Index: 0, CodecType: "video", Codec: "h264"},
			{Index: 1, CodecType: "video", Codec: "hevc"},
		},
		AudioTracks: []TrackInfo{
			{Index: 2, CodecType: "audio", Codec: "dts", Channels: 6, Language: "eng"},
			{Index: 3, CodecType: "audio", Codec: "ac3", Channels: 2, Title: "国语"},
			{Index: 4, CodecType: "audio", Codec: "aac", Channels: 2, Language: "jpn"},
		},
	}
	got := buildRenditions(probe, testBase())
	if len(got) != 5 {
		t.Fatalf("want 5 renditions, got %d: %+v", len(got), got)
	}
	if !got[0].IsVideo() || got[0].Name != "v0" || got[0].TrackIndex != 0 {
		t.Fatalf("bad first video rendition: %+v", got[0])
	}
	if !got[1].IsVideo() || got[1].Name != "v1" || got[1].TrackIndex != 1 {
		t.Fatalf("bad second video rendition: %+v", got[1])
	}
	if got[0].AudioCodec != "" || got[0].VideoBitrate == "" {
		t.Fatalf("video rendition must be audio-less: %+v", got[0])
	}
	if !got[2].IsAudio() || got[2].Name != "a0" || got[2].AudioCodec != "aac" {
		t.Fatalf("audio track must be forced to aac: %+v", got[2])
	}
	if got[2].AudioBitrate != "320k" {
		t.Fatalf("6ch should use 320k, got %s", got[2].AudioBitrate)
	}
	if got[4].AudioBitrate != "160k" {
		t.Fatalf("2ch should use 160k, got %s", got[4].AudioBitrate)
	}
	if got[4].VideoBitrate != "" || got[4].Scale != "" {
		t.Fatalf("audio rendition must not carry video settings: %+v", got[4])
	}
}

func TestMasterPlaylistMultiTrack(t *testing.T) {
	job := &Job{Profiles: []Profile{
		{Name: "v0", Kind: "video", VideoCodec: "h264", VideoBitrate: "4000k"},
		{Name: "v1", Kind: "video", VideoCodec: "h264", VideoBitrate: "4000k"},
		{Name: "a0", Kind: "audio", AudioCodec: "aac", AudioBitrate: "160k", Language: "eng"},
		{Name: "a1", Kind: "audio", AudioCodec: "aac", AudioBitrate: "160k", Title: "国语"},
	}}
	body := (&Cache{}).MasterPlaylist(job, "http://x/tc/j/t/")
	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud"`,
		`NAME="国语"`,
		`LANGUAGE="eng"`,
		`,AUDIO="aud"`,
		"http://x/tc/j/t/v0/playlist.m3u8",
		"http://x/tc/j/t/a1/playlist.m3u8",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("master playlist missing %q\n%s", want, body)
		}
	}
	// 只应有一条 DEFAULT=YES（第一条音轨）
	if strings.Count(body, "DEFAULT=YES") != 1 {
		t.Fatalf("expect exactly one DEFAULT=YES, got:\n%s", body)
	}
}

func TestParseSubtitleOrdinal(t *testing.T) {
	if n, ok := ParseSubtitleOrdinal("2.vtt"); !ok || n != 2 {
		t.Fatalf("parse 2.vtt failed: %d %v", n, ok)
	}
	if _, ok := ParseSubtitleOrdinal("2.srt"); ok {
		t.Fatal("should reject non-vtt")
	}
	if _, ok := ParseSubtitleOrdinal("../x.vtt"); ok {
		t.Fatal("should reject path traversal")
	}
}
