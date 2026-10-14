package transcode

import (
	"strings"
	"testing"
)

func TestBuildArgs(t *testing.T) {
	cfg := Config{
		OutputDir:  "/tmp/hls",
		SegmentSec: 2,
		ListSize:   6,
		Preset:     "veryfast",
		Renditions: []Rendition{
			{Name: "720p", Width: 1280, Height: 720, Bitrate: "2800k"},
			{Name: "480p", Width: 854, Height: 480, Bitrate: "1200k"},
		},
	}

	joined := strings.Join(buildArgs(cfg, "demo"), " ")
	for _, want := range []string{
		"-f flv -i pipe:0",
		"-filter_complex [0:v]split=2",
		"-var_stream_map v:0 v:1",
		"-master_pl_name index.m3u8",
		"/tmp/hls/demo/%v/stream.m3u8",
		"/tmp/hls/demo/%v/seg_%05d.ts",
		"-hls_time 2",
		"-hls_list_size 6",
		"-c:v:0 libx264",
		"-b:v:0 2800k",
		"-b:v:1 1200k",
		"-preset:v:0 veryfast",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q\nargs: %s", want, joined)
		}
	}
}

func TestDefaultLadder(t *testing.T) {
	l := DefaultLadder()
	if len(l) != 3 {
		t.Fatalf("ladder size = %d, want 3", len(l))
	}
	if l[0].Height != 1080 || l[1].Height != 720 || l[2].Height != 480 {
		t.Fatalf("unexpected ladder heights: %d, %d, %d", l[0].Height, l[1].Height, l[2].Height)
	}
}

func TestWithDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.FFmpegPath != "ffmpeg" || c.SegmentSec != 2 || c.ListSize != 6 || c.Preset != "veryfast" {
		t.Fatalf("defaults = %+v", c)
	}
	if len(c.Renditions) != 3 {
		t.Fatalf("renditions = %d, want 3", len(c.Renditions))
	}
}
