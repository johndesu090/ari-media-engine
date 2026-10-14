package rtmp

import (
	"encoding/binary"
	"errors"
)

// FLV video tag constants.
const (
	flvFrameKey     = 1
	flvCodecAVC     = 7
	avcSeqHeader    = 0
	avcNALU         = 1
	avcSeqEnd       = 2
	flvSoundAAC     = 10
	aacSeqHeader    = 0
	aacRaw          = 1
	defaultLenSize  = 4
	minAVCConfigLen = 7
)

// ErrMalformedTag indicates a truncated or invalid FLV tag body.
var ErrMalformedTag = errors.New("rtmp: malformed FLV tag")

// avcConfig is the decoded AVCDecoderConfigurationRecord.
type avcConfig struct {
	sps        []byte
	pps        []byte
	lengthSize int
	profileID  byte
	compat     byte
	level      byte
}

// parseAVCConfig decodes an AVCDecoderConfigurationRecord.
func parseAVCConfig(data []byte) (avcConfig, error) {
	var cfg avcConfig
	if len(data) < minAVCConfigLen {
		return cfg, ErrMalformedTag
	}
	cfg.profileID = data[1]
	cfg.compat = data[2]
	cfg.level = data[3]
	cfg.lengthSize = int(data[4]&0x03) + 1

	i := 6
	numSPS := int(data[5] & 0x1f)
	for s := 0; s < numSPS; s++ {
		if i+2 > len(data) {
			return cfg, ErrMalformedTag
		}
		n := int(binary.BigEndian.Uint16(data[i:]))
		i += 2
		if i+n > len(data) {
			return cfg, ErrMalformedTag
		}
		cfg.sps = append([]byte(nil), data[i:i+n]...)
		i += n
	}
	if i >= len(data) {
		return cfg, ErrMalformedTag
	}
	numPPS := int(data[i])
	i++
	for p := 0; p < numPPS; p++ {
		if i+2 > len(data) {
			return cfg, ErrMalformedTag
		}
		n := int(binary.BigEndian.Uint16(data[i:]))
		i += 2
		if i+n > len(data) {
			return cfg, ErrMalformedTag
		}
		cfg.pps = append([]byte(nil), data[i:i+n]...)
		i += n
	}
	if len(cfg.sps) == 0 || len(cfg.pps) == 0 {
		return cfg, ErrMalformedTag
	}
	return cfg, nil
}

// parseAVCCSample splits an AVCC (length-prefixed) sample into NAL units.
func parseAVCCSample(data []byte, lengthSize int) ([][]byte, error) {
	if lengthSize <= 0 {
		lengthSize = defaultLenSize
	}
	var nalus [][]byte
	for i := 0; i+lengthSize <= len(data); {
		var n int
		switch lengthSize {
		case 1:
			n = int(data[i])
		case 2:
			n = int(binary.BigEndian.Uint16(data[i:]))
		case 3:
			n = int(data[i])<<16 | int(data[i+1])<<8 | int(data[i+2])
		default:
			n = int(binary.BigEndian.Uint32(data[i:]))
		}
		i += lengthSize
		if n <= 0 || i+n > len(data) {
			break
		}
		nalus = append(nalus, data[i:i+n])
		i += n
	}
	return nalus, nil
}
