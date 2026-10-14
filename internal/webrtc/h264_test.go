package webrtc

import (
	"bytes"
	"testing"
)

func TestPacketizeH264SingleNALU(t *testing.T) {
	nalu := []byte{0x65, 0x01, 0x02, 0x03} // IDR slice, small
	var seq uint16
	packets := packetizeH264([][]byte{nalu}, &seq, 9000)

	if len(packets) != 1 {
		t.Fatalf("packets = %d, want 1", len(packets))
	}
	if !packets[0].Marker {
		t.Fatal("expected the marker bit on the last packet")
	}
	if !bytes.Equal(packets[0].Payload, nalu) {
		t.Fatalf("payload = %x", packets[0].Payload)
	}
	if packets[0].Timestamp != 9000 {
		t.Fatalf("timestamp = %d", packets[0].Timestamp)
	}
}

func TestPacketizeH264Fragmentation(t *testing.T) {
	nalu := make([]byte, h264MTU*2+50)
	nalu[0] = 0x65 // IDR
	for i := 1; i < len(nalu); i++ {
		nalu[i] = byte(i)
	}
	var seq uint16
	packets := packetizeH264([][]byte{nalu}, &seq, 0)

	if len(packets) < 3 {
		t.Fatalf("packets = %d, want >= 3 fragments", len(packets))
	}
	// FU-A indicator for the first fragment.
	if packets[0].Payload[0]&0x1f != 28 {
		t.Fatalf("FU-A type = %d, want 28", packets[0].Payload[0]&0x1f)
	}
	if packets[0].Payload[1]&0x80 == 0 {
		t.Fatal("expected the FU-A start bit on the first fragment")
	}
	last := packets[len(packets)-1]
	if last.Payload[1]&0x40 == 0 {
		t.Fatal("expected the FU-A end bit on the last fragment")
	}
	if !last.Marker {
		t.Fatal("expected the marker bit on the last fragment")
	}
	// Sequence numbers must increment monotonically.
	for i := 1; i < len(packets); i++ {
		if packets[i].SequenceNumber != packets[i-1].SequenceNumber+1 {
			t.Fatalf("sequence numbers not contiguous at %d", i)
		}
	}
}

func TestStapAPacket(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xE0, 0x1F}
	pps := []byte{0x68, 0xCE, 0x3C}
	var seq uint16
	p := stapAPacket(sps, pps, &seq, 0)

	if p.Payload[0]&0x1f != 24 {
		t.Fatalf("STAP-A type = %d, want 24", p.Payload[0]&0x1f)
	}
	// First NALU length + SPS.
	if int(p.Payload[1])<<8|int(p.Payload[2]) != len(sps) {
		t.Fatal("first NALU length mismatch")
	}
	if !bytes.Equal(p.Payload[3:3+len(sps)], sps) {
		t.Fatalf("sps = %x", p.Payload[3:3+len(sps)])
	}
	off := 3 + len(sps)
	if int(p.Payload[off])<<8|int(p.Payload[off+1]) != len(pps) {
		t.Fatal("second NALU length mismatch")
	}
	if !bytes.Equal(p.Payload[off+2:off+2+len(pps)], pps) {
		t.Fatalf("pps = %x", p.Payload[off+2:off+2+len(pps)])
	}
}
