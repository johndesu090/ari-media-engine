package rtsp

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// mediaDesc describes the first H.264 video track found in an SDP body.
type mediaDesc struct {
	payloadType int
	encoding    string
	clockRate   int
	control     string
	sps         []byte
	pps         []byte
}

// parseSDP extracts the first H.264 video media description from an SDP body.
func parseSDP(body string) (*mediaDesc, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")

	var md *mediaDesc
	inVideo := false
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "m=") {
			fields := strings.Fields(line[2:])
			if len(fields) >= 4 && fields[0] == "video" {
				inVideo = true
				md = &mediaDesc{}
				md.payloadType, _ = strconv.Atoi(fields[3])
			} else {
				inVideo = false
			}
			continue
		}
		if !inVideo || md == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "a=rtpmap:"):
			parseRTPMap(strings.TrimPrefix(line, "a=rtpmap:"), md)
		case strings.HasPrefix(line, "a=fmtp:"):
			parseFmtp(strings.TrimPrefix(line, "a=fmtp:"), md)
		case strings.HasPrefix(line, "a=control:"):
			md.control = strings.TrimSpace(strings.TrimPrefix(line, "a=control:"))
		}
	}

	if md == nil {
		return nil, errors.New("rtsp: no video track in SDP")
	}
	if !strings.EqualFold(md.encoding, "H264") {
		return nil, errors.New("rtsp: video track is not H.264 (" + md.encoding + ")")
	}
	if md.clockRate == 0 {
		md.clockRate = 90000
	}
	return md, nil
}

func parseRTPMap(val string, md *mediaDesc) {
	parts := strings.SplitN(val, " ", 2)
	if len(parts) != 2 {
		return
	}
	pt, err := strconv.Atoi(parts[0])
	if err != nil || pt != md.payloadType {
		return
	}
	enc := parts[1]
	if i := strings.IndexByte(enc, '/'); i >= 0 {
		md.encoding = enc[:i]
		rest := enc[i+1:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[:j]
		}
		md.clockRate, _ = strconv.Atoi(rest)
	} else {
		md.encoding = enc
	}
}

func parseFmtp(val string, md *mediaDesc) {
	parts := strings.SplitN(val, " ", 2)
	if len(parts) != 2 {
		return
	}
	if pt, err := strconv.Atoi(parts[0]); err != nil || pt != md.payloadType {
		return
	}
	for _, p := range strings.Split(parts[1], ";") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "sprop-parameter-sets=") {
			sets := strings.SplitN(strings.TrimPrefix(p, "sprop-parameter-sets="), ",", 2)
			if len(sets) == 2 {
				if b, err := base64.StdEncoding.DecodeString(sets[0]); err == nil {
					md.sps = b
				}
				if b, err := base64.StdEncoding.DecodeString(sets[1]); err == nil {
					md.pps = b
				}
			}
		}
	}
}

// resolveControl builds the absolute SETUP URL from the base RTSP URL and the
// SDP control attribute.
func resolveControl(base, control string) string {
	control = strings.TrimSpace(control)
	if control == "" {
		return base
	}
	if strings.HasPrefix(control, "rtsp://") || strings.HasPrefix(control, "rtsps://") {
		return control
	}
	if strings.HasSuffix(base, "/") {
		return base + control
	}
	return base + "/" + control
}
