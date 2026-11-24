// Tests ported from linux/tests/test_video_call.py and linux/tests/rtc_fake_edge.py in Meta's
// Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache
// License, Version 2.0 (see ../LICENSE), with changes.

package rtc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// A real 448x768 Constrained Baseline SPS/PPS from a Muse call (from the SDK's fake edge).
var (
	testSPS, _ = hex.DecodeString("6742c0298c648701848078442324")
	testPPS, _ = hex.DecodeString("68ce3c80")
	testIDR    = func() []byte {
		idr := []byte{0x65}
		for range 8 {
			for i := range 256 {
				idr = append(idr, byte(i))
			}
		}
		return idr
	}()
)

func stapA(nals ...[]byte) []byte {
	out := []byte{24}
	for _, nal := range nals {
		out = binary.BigEndian.AppendUint16(out, uint16(len(nal)))
		out = append(out, nal...)
	}
	return out
}

func fuA(header byte, chunk []byte) []byte { return append([]byte{0x7C, header}, chunk...) }

func videoPacket(seq uint16, payload []byte, marker bool) rtpPacket {
	return rtpPacket{payloadType: H264PayloadType, marker: marker, seq: seq, ssrc: ServerVideoSSRC, payload: payload}
}

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

var wantUnit = join(startCode, testSPS, startCode, testPPS, startCode, testIDR)

func TestH264DepacketizerWaitsForAKeyframeAndDropsBrokenUnits(t *testing.T) {
	d := newH264Depacketizer()
	if d.push(videoPacket(1, []byte{0x41, 1, 2}, true)) != nil {
		t.Fatal("a P frame before any IDR was handed out")
	}
	if d.push(videoPacket(2, stapA(testSPS, testPPS), false)) != nil {
		t.Fatal("unit before the marker")
	}
	if d.push(videoPacket(3, fuA(0x85, testIDR[1:100]), false)) != nil {
		t.Fatal("unit before the marker")
	}
	unit := d.push(videoPacket(4, fuA(0x45, testIDR[100:]), true))
	if !bytes.Equal(unit, wantUnit) {
		t.Fatalf("unit %x", unit[:min(len(unit), 32)])
	}
	if w, h, ok := spsDimensions(unit); !ok || w != 448 || h != 768 {
		t.Fatalf("size %dx%d %v", w, h, ok)
	}
	if got := d.push(videoPacket(5, []byte{0x41, 9}, true)); !bytes.Equal(got, join(startCode, []byte{0x41, 9})) {
		t.Fatalf("P frame %x", got)
	}
	if d.push(videoPacket(7, []byte{0x41, 9}, true)) != nil { // seq 6 lost
		t.Fatal("a unit after loss was handed out")
	}
	if !d.needsKey {
		t.Fatal("loss did not ask for a keyframe")
	}
}

func TestH264DepacketizerCapsUnitSize(t *testing.T) {
	d := newH264Depacketizer()
	chunk := fuA(0x05, make([]byte, 60000))
	seq := uint16(0)
	d.push(videoPacket(seq, fuA(0x85, make([]byte, 60000)), false))
	for d.size > 0 && d.size <= maxUnitBytes {
		seq++
		d.push(videoPacket(seq, chunk, false))
	}
	if d.size != 0 || !d.needsKey {
		t.Fatal("an oversized unit was buffered")
	}
	seq++
	if d.push(videoPacket(seq, fuA(0x45, nil), true)) != nil {
		t.Fatal("an oversized unit was handed out")
	}
}

func TestH264FragmentEdgeCasesRequestAKeyframe(t *testing.T) {
	d := newH264Depacketizer()
	d.push(videoPacket(1, stapA(testSPS, testPPS), false))
	d.push(videoPacket(2, fuA(0x85, testIDR[1:]), false))
	unit := d.push(videoPacket(3, fuA(0x45, nil), true)) // empty end fragment
	if !bytes.Equal(unit, wantUnit) || d.needsKey {
		t.Fatal("empty end fragment")
	}
	if d.push(videoPacket(4, fuA(0x01, []byte("x")), true)) != nil || !d.needsKey { // no start
		t.Fatal("a fragment without its start")
	}
	d.push(videoPacket(5, stapA(testSPS, testPPS), false))
	d.push(videoPacket(6, fuA(0x85, testIDR[1:]), false))
	if d.push(videoPacket(7, []byte{0x41, 9}, true)) != nil || !d.needsKey { // unfinished at the marker
		t.Fatal("a fragment unfinished at the marker")
	}
}

func TestH264SequenceWrapIsNotLoss(t *testing.T) {
	d := newH264Depacketizer()
	d.push(videoPacket(65535, stapA(testSPS, testPPS), false))
	if unit := d.push(videoPacket(0, testIDR, true)); !bytes.Equal(unit, wantUnit) {
		t.Fatal("a unit across the sequence wrap was dropped")
	}
}

func TestMalformedSPSHasNoSize(t *testing.T) {
	longZeros := append([]byte{0x67, 0x42, 0xC0, 0x29}, make([]byte, 64)...) // an Exp-Golomb code with no end
	if _, _, ok := spsDimensions(join(startCode, longZeros)); ok {
		t.Fatal("size from a malformed SPS")
	}
	if _, _, ok := spsDimensions(join(startCode, []byte{0x67, 0x64})); ok {
		t.Fatal("size from a truncated SPS")
	}
	if _, _, ok := spsDimensions(join(startCode, []byte{0x41, 1})); ok {
		t.Fatal("size without an SPS")
	}
}

func TestParseRTP(t *testing.T) {
	p := buildRTP(OpusPayloadType, 7, 960, ServerAudioSSRC, []byte("abc"), true)
	got, err := parseRTP(p)
	if err != nil || got.payloadType != 111 || !got.marker || got.seq != 7 || got.timestamp != 960 ||
		got.ssrc != 200 || string(got.payload) != "abc" {
		t.Fatalf("%+v %v", got, err)
	}
	// One CSRC, an extension and two bytes of padding.
	padded := []byte{0xB1, 111, 0, 1, 0, 0, 0, 0, 0, 0, 0, 200, 9, 9, 9, 9, 0xBE, 0xDE, 0, 1, 1, 2, 3, 4, 'h', 'i', 0, 2}
	got, err = parseRTP(padded)
	if err != nil || string(got.payload) != "hi" {
		t.Fatalf("padded: %q %v", got.payload, err)
	}
	for _, bad := range [][]byte{{0x80, 0}, make([]byte, 12), {0x90, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, {0xA0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 200}} {
		if _, err := parseRTP(bad); err == nil {
			t.Fatalf("accepted % x", bad)
		}
	}
}

func TestPLIAndRTCPDetection(t *testing.T) {
	pli := buildPLI(ClientVideoSSRC, ServerVideoSSRC)
	if !bytes.Equal(pli, []byte{0x81, 206, 0, 2, 0, 0, 0, 101, 0, 0, 0, 201}) {
		t.Fatalf("PLI % x", pli)
	}
	if !isRTCP(pli) || isRTCP(buildRTP(111, 1, 1, 200, nil, true)) || isRTCP(buildRTP(127, 1, 1, 201, nil, false)) {
		t.Fatal("RTCP detection")
	}
}
