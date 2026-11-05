package cscall

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestRTPHeadersAndPadding(t *testing.T) {
	packet := make([]byte, 12+4+4+4+3+2)
	packet[0] = 0xb1
	packet[1] = 0
	binary.BigEndian.PutUint16(packet[18:20], 1)
	copy(packet[24:], []byte{0xff, 0x7f, 0x00})
	packet[len(packet)-1] = 2
	p, err := RTPPayload(packet)
	if err != nil || !bytes.Equal(p, []byte{0xff, 0x7f, 0}) {
		t.Fatalf("payload %x err %v", p, err)
	}
	for _, bad := range [][]byte{{}, packet[:15], packet[:19], append([]byte{0}, packet[1:]...)} {
		if _, err := RTPPayload(bad); err == nil {
			t.Fatal("accepted malformed RTP")
		}
	}
}
func TestG711GoldenAndTimestamps(t *testing.T) {
	got := DecodeUlawToPCM([]byte{0xff, 0x00})
	if !bytes.Equal(got, []byte{0, 0, 0x84, 0x82}) {
		t.Fatalf("G711 decode: %x", got)
	}
	ab := &AudioBridge{ssrc: 123, seqNum: 65535, timestamp: 100}
	a := ab.buildRTPPacket(make([]byte, 160))
	b := ab.buildRTPPacket(make([]byte, 160))
	if binary.BigEndian.Uint16(a[2:4]) != 65535 || binary.BigEndian.Uint16(b[2:4]) != 0 || binary.BigEndian.Uint32(b[4:8]) != 260 {
		t.Fatal("RTP sequence/timestamp wrong")
	}
}
func TestNumberInjectionRejected(t *testing.T) {
	for _, s := range []string{"123;ATH", "123\rATD456;", "sip:123", ""} {
		if validNumber(s) {
			t.Fatalf("accepted %q", s)
		}
	}
	if !validNumber("+8613800000000") {
		t.Fatal("valid number rejected")
	}
}
