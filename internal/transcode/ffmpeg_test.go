package transcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildHLSArgs(t *testing.T) {
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

	joined := strings.Join(buildHLSArgs(cfg, "demo", "T"), " ")
	for _, want := range []string{
		"-f flv -i pipe:0",
		"-filter_complex [0:v]split=2",
		"-var_stream_map v:0,a:0 v:1,a:1",
		"-map 0:a:0? -c:a:0 aac",
		"-c:a:1 aac",
		"/tmp/hls/demo/%v/stream.m3u8",
		"/tmp/hls/demo/%v/seg_T_%05d.ts",
		"-hls_time 2",
		"-hls_list_size 6",
		"-c:v:0 libx264",
		"-b:v:0 2800k",
		"-b:v:1 1200k",
		"-preset:v:0 veryfast",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("HLS args missing %q\nargs: %s", want, joined)
		}
	}
}

func TestBuildDASHArgs(t *testing.T) {
	cfg := Config{
		OutputDir:  "/tmp/hls",
		SegmentSec: 2,
		Preset:     "veryfast",
		Renditions: []Rendition{{Name: "720p", Width: 1280, Height: 720, Bitrate: "2800k"}},
	}

	joined := strings.Join(buildDASHArgs(cfg, "demo", "T"), " ")
	for _, want := range []string{
		"-f flv -i pipe:0",
		"-f dash",
		"-seg_duration 2",
		"-adaptation_sets id=0,streams=v id=1,streams=a",
		"-media_seg_name chunk-T-$RepresentationID$-$Number%05d$.m4s",
		"/tmp/hls/demo/manifest.mpd",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("DASH args missing %q\nargs: %s", want, joined)
		}
	}
}

func TestBuildCopyHLSArgs(t *testing.T) {
	cfg := Config{OutputDir: "/tmp/hls", SegmentSec: 2, ListSize: 6}
	joined := strings.Join(buildCopyHLSArgs(cfg, "demo", "T"), " ")
	for _, want := range []string{
		"-f flv -i pipe:0",
		"-c copy",
		"-bsf:v h264_mp4toannexb",
		"-f hls",
		"/tmp/hls/demo/index.m3u8",
		"/tmp/hls/demo/seg_T_%05d.ts",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("copy HLS args missing %q\nargs: %s", want, joined)
		}
	}
	if strings.Contains(joined, "libx264") {
		t.Fatalf("copy HLS args should not re-encode: %s", joined)
	}
}

func TestBuildCopyDASHArgs(t *testing.T) {
	cfg := Config{OutputDir: "/tmp/hls", SegmentSec: 2}
	joined := strings.Join(buildCopyDASHArgs(cfg, "demo", "T"), " ")
	for _, want := range []string{
		"-f flv -i pipe:0",
		"-c copy",
		"-f dash",
		"-adaptation_sets id=0,streams=v id=1,streams=a",
		"-media_seg_name chunk-T-$RepresentationID$-$Number%05d$.m4s",
		"/tmp/hls/demo/manifest.mpd",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("copy DASH args missing %q\nargs: %s", want, joined)
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

func TestWriteHLSMaster(t *testing.T) {
	dir := t.TempDir()
	if err := writeHLSMaster(dir, []Rendition{
		{Width: 1280, Height: 720, Bitrate: "2800k"},
		{Width: 854, Height: 480, Bitrate: "1200k"},
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"RESOLUTION=1280x720", "RESOLUTION=854x480", "0/stream.m3u8", "1/stream.m3u8", "BANDWIDTH=2800000"} {
		if !strings.Contains(s, want) {
			t.Fatalf("master playlist missing %q:\n%s", want, s)
		}
	}
}

func TestParseBitrate(t *testing.T) {
	cases := map[string]int{"2800k": 2800000, "5m": 5000000, "1200k": 1200000, "900": 900, "": 0, "abc": 0}
	for in, want := range cases {
		if got := parseBitrate(in); got != want {
			t.Errorf("parseBitrate(%q) = %d, want %d", in, got, want)
		}
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
