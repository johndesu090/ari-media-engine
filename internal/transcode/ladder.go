// Package transcode runs an FFmpeg-based adaptive-bitrate ladder for ingested
// streams. A publisher's raw FLV stream is piped to FFmpeg, which transcodes it
// into multiple HLS renditions (1080p/720p/480p by default) under the configured
// output directory, ready to be served by the engine's /hls handler.
package transcode

// Rendition is one rung of the ABR ladder.
type Rendition struct {
	Name    string
	Width   int
	Height  int
	Bitrate string
	MaxRate string
	BufSize string
}

// Config configures the transcode Manager.
type Config struct {
	// FFmpegPath is the ffmpeg executable (default "ffmpeg").
	FFmpegPath string
	// OutputDir is the HLS root; each stream is written to <OutputDir>/<key>/.
	OutputDir string
	// SegmentSec is the HLS target segment duration.
	SegmentSec int
	// ListSize is the number of segments kept in the live playlist.
	ListSize int
	// Preset is the x264 preset.
	Preset string
	// Abr enables the multi-rendition ladder. When false the stream is remuxed
	// with -c copy so playback follows the publisher's encoder settings.
	Abr bool
	// Renditions is the ABR ladder, highest quality first.
	Renditions []Rendition
}

// DefaultLadder returns the standard 1080p/720p/480p ladder.
func DefaultLadder() []Rendition {
	return []Rendition{
		{Name: "1080p", Width: 1920, Height: 1080, Bitrate: "5000k", MaxRate: "5350k", BufSize: "7500k"},
		{Name: "720p", Width: 1280, Height: 720, Bitrate: "2800k", MaxRate: "2996k", BufSize: "4200k"},
		{Name: "480p", Width: 854, Height: 480, Bitrate: "1200k", MaxRate: "1284k", BufSize: "1800k"},
	}
}

func (c Config) withDefaults() Config {
	if c.FFmpegPath == "" {
		c.FFmpegPath = "ffmpeg"
	}
	if c.OutputDir == "" {
		c.OutputDir = "./data/hls"
	}
	if c.SegmentSec <= 0 {
		c.SegmentSec = 2
	}
	if c.ListSize <= 0 {
		c.ListSize = 6
	}
	if c.Preset == "" {
		c.Preset = "veryfast"
	}
	if len(c.Renditions) == 0 {
		c.Renditions = DefaultLadder()
	}
	return c
}
