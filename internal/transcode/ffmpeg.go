package transcode

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Format selects the packaging format(s) the transcoder produces.
type Format uint8

const (
	// FormatHLS produces a multi-variant HLS ladder.
	FormatHLS Format = 1 << iota
	// FormatDASH produces a DASH MPD with one video adaptation set.
	FormatDASH
)

// filterGraph builds the split + scale video graph for the ladder.
func filterGraph(renditions []Rendition) string {
	var fb strings.Builder
	fmt.Fprintf(&fb, "[0:v]split=%d", len(renditions))
	for i := range renditions {
		fmt.Fprintf(&fb, "[v%d]", i)
	}
	for i, r := range renditions {
		fmt.Fprintf(&fb, ";[v%d]scale=w=%d:h=%d:force_original_aspect_ratio=decrease[v%dout]", i, r.Width, r.Height, i)
	}
	return fb.String()
}

// encodeArgs returns the per-rendition map/encode options shared by all formats.
func encodeArgs(cfg Config) []string {
	var args []string
	for i, r := range cfg.Renditions {
		args = append(args,
			"-map", fmt.Sprintf("[v%dout]", i),
			fmt.Sprintf("-c:v:%d", i), "libx264",
			fmt.Sprintf("-b:v:%d", i), r.Bitrate,
			fmt.Sprintf("-preset:v:%d", i), cfg.Preset,
		)
		if r.MaxRate != "" {
			args = append(args, fmt.Sprintf("-maxrate:v:%d", i), r.MaxRate)
		}
		if r.BufSize != "" {
			args = append(args, fmt.Sprintf("-bufsize:v:%d", i), r.BufSize)
		}
	}
	gop := strconv.Itoa(cfg.SegmentSec * 24)
	return append(args,
		"-g", gop,
		"-keyint_min", gop,
		"-sc_threshold", "0",
		"-pix_fmt", "yuv420p",
	)
}

// buildHLSArgs builds the FFmpeg arguments for the HLS ladder.
func buildHLSArgs(cfg Config, streamKey string) []string {
	outDir := filepath.ToSlash(filepath.Join(cfg.OutputDir, streamKey))

	var streamMap []string
	for i := range cfg.Renditions {
		streamMap = append(streamMap, fmt.Sprintf("v:%d", i))
	}

	args := []string{"-hide_banner", "-loglevel", "warning", "-probesize", "500000", "-analyzeduration", "1000000",
		"-f", "flv", "-i", "pipe:0",
		"-filter_complex", filterGraph(cfg.Renditions)}
	args = append(args, encodeArgs(cfg)...)
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(cfg.SegmentSec),
		"-hls_list_size", strconv.Itoa(cfg.ListSize),
		"-hls_flags", "delete_segments+independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", outDir+"/%v/seg_%05d.ts",
		"-var_stream_map", strings.Join(streamMap, " "),
		outDir+"/%v/stream.m3u8",
	)
	return args
}

// writeHLSMaster writes the multi-variant master playlist. FFmpeg only writes
// the master playlist on exit, so the engine generates it up front from the
// fixed ladder; this keeps it available for live playback immediately.
func writeHLSMaster(dir string, renditions []Rendition) error {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i, r := range renditions {
		bw := parseBitrate(r.Bitrate)
		if m := parseBitrate(r.MaxRate); m > bw {
			bw = m
		}
		if bw == 0 {
			bw = 1_000_000
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d\n", bw, r.Width, r.Height)
		fmt.Fprintf(&b, "%d/stream.m3u8\n", i)
	}
	return os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(b.String()), 0o644)
}

// parseBitrate converts a bitrate like "2800k" or "5m" to bits per second.
func parseBitrate(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1000, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "m")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n * mult
}

// buildDASHArgs builds the FFmpeg arguments for the DASH MPD.
func buildDASHArgs(cfg Config, streamKey string) []string {
	outDir := filepath.ToSlash(filepath.Join(cfg.OutputDir, streamKey))

	args := []string{"-hide_banner", "-loglevel", "warning", "-probesize", "500000", "-analyzeduration", "1000000",
		"-f", "flv", "-i", "pipe:0",
		"-filter_complex", filterGraph(cfg.Renditions)}
	args = append(args, encodeArgs(cfg)...)
	args = append(args,
		"-f", "dash",
		"-seg_duration", strconv.Itoa(cfg.SegmentSec),
		"-use_template", "1",
		"-use_timeline", "1",
		"-adaptation_sets", "id=0,streams=v",
		"-init_seg_name", "init-$RepresentationID$.m4s",
		"-media_seg_name", "chunk-$RepresentationID$-$Number%05d$.m4s",
		outDir+"/manifest.mpd",
	)
	return args
}
