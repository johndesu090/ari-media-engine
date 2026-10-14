// Package srt implements SRT ingest. A publisher (OBS, ffmpeg, hardware) connects
// over SRT carrying MPEG-TS; this package demuxes the transport stream to H.264
// and feeds the WebRTC engine's ingest session.
package srt

import "encoding/binary"

const tsPacketSize = 188

// accessUnitFunc receives a decoded H.264 access unit.
type accessUnitFunc func(nalus [][]byte, ptsMillis uint32, keyframe bool)

// tsDemuxer parses an MPEG-TS byte stream, discovers the H.264 elementary
// stream via PAT/PMT, reassembles PES packets and emits H.264 access units.
// It assumes one access unit per PES packet (the common live muxing case).
type tsDemuxer struct {
	pmtPID   int
	videoPID int

	// Partial TS packet carry-over between pushes.
	buf []byte

	// PES reassembly for the video PID.
	pesBuf    []byte
	pesPTS    uint32
	pesActive bool

	onAccessUnit accessUnitFunc
}

func newTSDemuxer(onAU accessUnitFunc) *tsDemuxer {
	return &tsDemuxer{pmtPID: -1, videoPID: -1, onAccessUnit: onAU}
}

// push feeds arbitrary-length TS bytes; partial packets are buffered.
func (d *tsDemuxer) push(data []byte) {
	d.buf = append(d.buf, data...)
	for len(d.buf) >= tsPacketSize {
		if d.buf[0] != 0x47 {
			i := 1
			for i < len(d.buf) && d.buf[i] != 0x47 {
				i++
			}
			d.buf = d.buf[i:]
			continue
		}
		d.packet(d.buf[:tsPacketSize])
		d.buf = d.buf[tsPacketSize:]
	}
}

func (d *tsDemuxer) packet(pkt []byte) {
	pusi := pkt[1]&0x40 != 0
	pid := int(pkt[1]&0x1f)<<8 | int(pkt[2])
	afc := (pkt[3] >> 4) & 0x03

	offset := 4
	switch afc {
	case 0:
		return
	case 2: // adaptation field only, no payload
		return
	case 3:
		if offset >= len(pkt) {
			return
		}
		offset += 1 + int(pkt[offset])
	}
	if offset >= len(pkt) {
		return
	}
	payload := pkt[offset:]

	switch {
	case pid == 0:
		d.parseSection(payload, pusi, d.parsePAT)
	case pid == d.pmtPID:
		d.parseSection(payload, pusi, d.parsePMT)
	case pid == d.videoPID:
		d.parsePES(payload, pusi)
	}
}

func (d *tsDemuxer) parseSection(payload []byte, pusi bool, parse func([]byte)) {
	if !pusi || len(payload) < 4 {
		return
	}
	ptr := int(payload[0])
	if 1+ptr+3 > len(payload) {
		return
	}
	sec := payload[1+ptr:]
	slen := int(binary.BigEndian.Uint16(sec[1:3]) & 0x0fff)
	if 3+slen > len(sec) {
		return // multi-packet section; ignore
	}
	parse(sec[:3+slen])
}

func (d *tsDemuxer) parsePAT(sec []byte) {
	if sec[0] != 0x00 || len(sec) < 12 {
		return
	}
	for i := 8; i+4 <= len(sec)-4; i += 4 {
		program := binary.BigEndian.Uint16(sec[i:])
		pid := int(binary.BigEndian.Uint16(sec[i+2:]) & 0x1fff)
		if program != 0 {
			d.pmtPID = pid
			return
		}
	}
}

func (d *tsDemuxer) parsePMT(sec []byte) {
	if sec[0] != 0x02 || len(sec) < 16 {
		return
	}
	progInfoLen := int(binary.BigEndian.Uint16(sec[10:]) & 0x0fff)
	i := 12 + progInfoLen
	for i+5 <= len(sec)-4 {
		streamType := sec[i]
		pid := int(binary.BigEndian.Uint16(sec[i+1:]) & 0x1fff)
		esInfoLen := int(binary.BigEndian.Uint16(sec[i+3:]) & 0x0fff)
		i += 5 + esInfoLen
		if streamType == 0x1b { // H.264
			d.videoPID = pid
		}
	}
}

func (d *tsDemuxer) parsePES(payload []byte, pusi bool) {
	if pusi {
		d.flushPES()
		if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
			return
		}
		flags := payload[7]
		headerDataLen := int(payload[8])
		if flags&0x80 != 0 && len(payload) >= 14 {
			d.pesPTS = parsePTS(payload[9:14])
		} else {
			d.pesPTS = 0
		}
		start := 9 + headerDataLen
		if start <= len(payload) {
			d.pesBuf = append(d.pesBuf[:0], payload[start:]...)
			d.pesActive = true
		}
		return
	}
	if d.pesActive {
		d.pesBuf = append(d.pesBuf, payload...)
	}
}

func (d *tsDemuxer) flushPES() {
	if !d.pesActive {
		return
	}
	d.pesActive = false
	nalus := splitAnnexB(d.pesBuf)
	if len(nalus) == 0 {
		return
	}
	keyframe := false
	for _, n := range nalus {
		if len(n) > 0 && n[0]&0x1f == 5 {
			keyframe = true
		}
	}
	d.onAccessUnit(nalus, d.pesPTS/90, keyframe)
}

// parsePTS decodes a 33-bit PTS from a 5-byte PES timestamp field.
func parsePTS(b []byte) uint32 {
	return (uint32(b[0]&0x0e) << 29) |
		(uint32(b[1]) << 22) |
		(uint32(b[2]&0xfe) << 14) |
		(uint32(b[3]) << 7) |
		(uint32(b[4]) >> 1)
}
