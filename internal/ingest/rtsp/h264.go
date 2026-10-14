package rtsp

// h264Depacketizer reassembles H.264 NAL units from RTP payloads per RFC 6184.
// It supports single NAL units, STAP-A aggregation and FU-A fragmentation, which
// covers the common RTSP camera and server cases.
type h264Depacketizer struct {
	fuBuffer []byte
	fuActive bool
}

// push feeds one RTP payload and returns the NAL units it completed, plus
// whether any of them is a keyframe (IDR, NAL type 5).
func (d *h264Depacketizer) push(payload []byte) ([][]byte, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	nalType := payload[0] & 0x1f

	switch {
	case nalType >= 1 && nalType <= 23:
		return [][]byte{append([]byte(nil), payload...)}, nalType == 5

	case nalType == 24: // STAP-A
		var nalus [][]byte
		keyframe := false
		for i := 1; i+2 <= len(payload); {
			n := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if n <= 0 || i+n > len(payload) {
				break
			}
			nalu := append([]byte(nil), payload[i:i+n]...)
			nalus = append(nalus, nalu)
			if len(nalu) > 0 && nalu[0]&0x1f == 5 {
				keyframe = true
			}
			i += n
		}
		return nalus, keyframe

	case nalType == 28: // FU-A
		if len(payload) < 2 {
			return nil, false
		}
		indicator := payload[0]
		fuHeader := payload[1]
		start := fuHeader&0x80 != 0
		end := fuHeader&0x40 != 0
		origType := fuHeader & 0x1f

		if start {
			d.fuBuffer = []byte{(indicator & 0xe0) | origType}
			d.fuActive = true
		}
		if d.fuActive {
			d.fuBuffer = append(d.fuBuffer, payload[2:]...)
		}
		if end && d.fuActive {
			nalu := d.fuBuffer
			d.fuBuffer = nil
			d.fuActive = false
			return [][]byte{nalu}, origType == 5
		}
		return nil, false

	default:
		return nil, false
	}
}
