package rtmp

import (
	"bytes"
	"testing"
)

func buildAVCConfig(sps, pps []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x01, 0x42, 0xE0, 0x1F, 0xFF, 0xE1}) // version, profile, compat, level, lenSizeMinus1=3, numSPS=1
	b.Write([]byte{byte(len(sps) >> 8), byte(len(sps))})
	b.Write(sps)
	b.WriteByte(0x01) // numPPS
	b.Write([]byte{byte(len(pps) >> 8), byte(len(pps))})
	b.Write(pps)
	return b.Bytes()
}

func TestParseAVCConfig(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xE0, 0x1F, 0xDA}
	pps := []byte{0x68, 0xCE, 0x3C}
	cfg, err := parseAVCConfig(buildAVCConfig(sps, pps))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.lengthSize != 4 {
		t.Fatalf("lengthSize = %d, want 4", cfg.lengthSize)
	}
	if !bytes.Equal(cfg.sps, sps) {
		t.Fatalf("sps = %x", cfg.sps)
	}
	if !bytes.Equal(cfg.pps, pps) {
		t.Fatalf("pps = %x", cfg.pps)
	}
	if cfg.profileID != 0x42 || cfg.level != 0x1F {
		t.Fatalf("profile/level = %x/%x", cfg.profileID, cfg.level)
	}
}

func TestParseAVCCSample(t *testing.T) {
	var b bytes.Buffer
	b.Write([]byte{0x00, 0x00, 0x00, 0x03, 0x65, 0xAA, 0xBB}) // NALU 1 (3 bytes)
	b.Write([]byte{0x00, 0x00, 0x00, 0x02, 0x41, 0xCC})       // NALU 2 (2 bytes)

	nalus, err := parseAVCCSample(b.Bytes(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(nalus) != 2 {
		t.Fatalf("nalus = %d, want 2", len(nalus))
	}
	if !bytes.Equal(nalus[0], []byte{0x65, 0xAA, 0xBB}) {
		t.Fatalf("nalu[0] = %x", nalus[0])
	}
	if !bytes.Equal(nalus[1], []byte{0x41, 0xCC}) {
		t.Fatalf("nalu[1] = %x", nalus[1])
	}
}

func TestParseAVCConfigMalformed(t *testing.T) {
	if _, err := parseAVCConfig([]byte{0x01, 0x02}); err == nil {
		t.Fatal("expected an error for a truncated config record")
	}
}
