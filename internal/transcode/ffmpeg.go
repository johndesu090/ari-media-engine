package transcode

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// buildArgs constructs the FFmpeg argument list that reads FLV from stdin and
// writes an ABR HLS ladder into <OutputDir>/<streamKey>/. The master playlist is
// named index.m3u8 so it matches the player's /hls/<streamID>/index.m3u8 URL.
func buildArgs(cfg Config, streamKey string) []string {
	outDir := filepath.ToSlash(filepath.Join(cfg.OutputDir, streamKey))

	// Video split + scale graph: [0:v] -> N scaled branches.
	var fb strings.Builder
	fmt.Fprintf(&fb, "[0:v]split=%d", len(cfg.Renditions))
	for i := range cfg.Renditions {
		fmt.Fprintf(&fb, "[v%d]", i)
	}
	for i, r := range cfg.Renditions {
		fmt.Fprintf(&fb, ";[v%d]scale=w=%d:h=%d:force_original_aspect_ratio=decrease[v%dout]", i, r.Width, r.Height, i)
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-f", "flv",
		"-i", "pipe:0",
		"-filter_complex", fb.String(),
	}

	var streamMap []string
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
		streamMap = append(streamMap, fmt.Sprintf("v:%d", i))
	}

	// Keyframe alignment across renditions (required for seamless ABR switching).
	gop := strconv.Itoa(cfg.SegmentSec * 24)
	args = append(args,
		"-g", gop,
		"-keyint_min", gop,
		"-sc_threshold", "0",
		"-pix_fmt", "yuv420p",
	)

	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(cfg.SegmentSec),
		"-hls_list_size", strconv.Itoa(cfg.ListSize),
		"-hls_flags", "delete_segments+independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", outDir+"/%v/seg_%05d.ts",
		"-master_pl_name", "index.m3u8",
		"-var_stream_map", strings.Join(streamMap, " "),
		outDir+"/%v/stream.m3u8",
	)
	return args
}
