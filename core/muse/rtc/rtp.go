// RTP parsing, H.264 depacketization (RFC 6184) and small RTCP builders. This is a modified port
// of linux/src/musegadget/rtc/rtp.py from Meta's Muse Gadget SDK, Copyright (c) Meta Platforms,
// Inc. and affiliates, licensed under the Apache License, Version 2.0 (see ../LICENSE).

package rtc

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// startCode prefixes each NAL unit in an Annex-B access unit.
var startCode = []byte{0, 0, 0, 1}

// isRTCP reports whether a packet's second byte is an RTCP packet type (192..223).
func isRTCP(data []byte) bool { return len(data) >= 8 && data[1] >= 192 && data[1] <= 223 }

// rtpPacket is a parsed RTP packet; payload aliases the parsed buffer.
type rtpPacket struct {
	payloadType uint8
	marker      bool
	seq         uint16
	timestamp   uint32
	ssrc        uint32
	payload     []byte
}

var errNotRTP = errors.New("rtc: not RTP")

func parseRTP(data []byte) (rtpPacket, error) {
	if len(data) < 12 || data[0]>>6 != 2 {
		return rtpPacket{}, errNotRTP
	}
	offset := 12 + 4*int(data[0]&0x0F)
	if data[0]&0x10 != 0 {
		if len(data) < offset+4 {
			return rtpPacket{}, errors.New("rtc: bad RTP lengths")
		}
		offset += 4 + 4*int(binary.BigEndian.Uint16(data[offset+2:]))
	}
	end := len(data)
	if data[0]&0x20 != 0 {
		end -= int(data[len(data)-1])
	}
	if offset > end {
		return rtpPacket{}, errors.New("rtc: bad RTP lengths")
	}
	return rtpPacket{
		payloadType: data[1] & 0x7F,
		marker:      data[1]&0x80 != 0,
		seq:         binary.BigEndian.Uint16(data[2:]),
		timestamp:   binary.BigEndian.Uint32(data[4:]),
		ssrc:        binary.BigEndian.Uint32(data[8:]),
		payload:     data[offset:end],
	}, nil
}

func buildRTP(payloadType uint8, seq uint16, timestamp, ssrc uint32, payload []byte, marker bool) []byte {
	out := make([]byte, 12, 12+len(payload))
	out[0] = 0x80
	out[1] = payloadType
	if marker {
		out[1] |= 0x80
	}
	binary.BigEndian.PutUint16(out[2:], seq)
	binary.BigEndian.PutUint32(out[4:], timestamp)
	binary.BigEndian.PutUint32(out[8:], ssrc)
	return append(out, payload...)
}

// buildPLI builds an RTCP Picture Loss Indication (RFC 4585): ask the sender for a keyframe.
func buildPLI(senderSSRC, mediaSSRC uint32) []byte {
	out := []byte{0x81, 206, 0, 2}
	out = binary.BigEndian.AppendUint32(out, senderSSRC)
	return binary.BigEndian.AppendUint32(out, mediaSSRC)
}

// Limits on one reassembled access unit.
const (
	maxUnitBytes = 2 * 1024 * 1024
	maxUnitNALs  = 256
)

// h264Depacketizer reassembles RTP H.264 (single NAL, STAP-A, FU-A) into Annex-B access units,
// one per RTP marker. It drops a unit on loss or overflow and then waits for the next keyframe
// (IDR), so it never hands out a unit that cannot decode. It is not safe for concurrent use.
type h264Depacketizer struct {
	nals         [][]byte
	size         int
	fu           []byte // the fragmented NAL in progress, or nil
	expectedSeq  uint16
	haveExpected bool
	broken       bool
	needsKey     bool
}

func newH264Depacketizer() *h264Depacketizer { return &h264Depacketizer{needsKey: true} }

// push adds one packet and returns a complete access unit at the marker, or nil.
func (d *h264Depacketizer) push(p rtpPacket) []byte {
	if d.haveExpected && p.seq != d.expectedSeq {
		d.drop()
	}
	d.expectedSeq, d.haveExpected = p.seq+1, true
	if !d.broken {
		d.consume(p.payload)
		if d.size > maxUnitBytes || len(d.nals) > maxUnitNALs {
			d.drop()
		}
	}
	if !p.marker {
		return nil
	}
	if d.fu != nil { // a fragmented NAL left unfinished at the marker
		d.drop()
	}
	nals, broken := d.nals, d.broken
	d.nals, d.size, d.fu, d.broken = nil, 0, nil, false
	if broken || len(nals) == 0 {
		return nil
	}
	for _, nal := range nals {
		if nal[0]&0x1F == 5 {
			d.needsKey = false
		}
	}
	if d.needsKey {
		return nil
	}
	return annexB(nals)
}

func annexB(nals [][]byte) []byte {
	total := 0
	for _, nal := range nals {
		total += len(startCode) + len(nal)
	}
	out := make([]byte, 0, total)
	for _, nal := range nals {
		out = append(append(out, startCode...), nal...)
	}
	return out
}

// drop discards the unit in progress; the rest of it arrives up to the marker.
func (d *h264Depacketizer) drop() {
	d.nals, d.size, d.fu = nil, 0, nil
	d.broken, d.needsKey = true, true
}

func (d *h264Depacketizer) consume(payload []byte) {
	if len(payload) == 0 {
		return
	}
	d.size += len(payload)
	switch kind := payload[0] & 0x1F; {
	case kind >= 1 && kind <= 23: // a single NAL unit
		d.nals = append(d.nals, payload)
	case kind == 24: // STAP-A: 16-bit size, NAL, repeated
		for offset := 1; offset+2 <= len(payload); {
			size := int(binary.BigEndian.Uint16(payload[offset:]))
			offset += 2
			if size > 0 && offset+size <= len(payload) {
				d.nals = append(d.nals, payload[offset:offset+size])
			}
			offset += size
		}
	case kind == 28 && len(payload) >= 2: // FU-A; the fragment itself may be empty
		start, end := payload[1]&0x80 != 0, payload[1]&0x40 != 0
		if start {
			d.fu = []byte{payload[0]&0xE0 | payload[1]&0x1F}
		}
		if d.fu == nil { // a fragment without its start
			d.drop()
			return
		}
		d.fu = append(d.fu, payload[2:]...)
		if end {
			d.nals = append(d.nals, d.fu)
			d.fu = nil
		}
	}
}

// spsDimensions returns the picture size from the first SPS in an Annex-B access unit.
func spsDimensions(accessUnit []byte) (width, height int, ok bool) {
	for _, nal := range bytes.Split(accessUnit, startCode) {
		if len(nal) > 0 && nal[0]&0x1F == 7 {
			w, h, err := parseSPS(nal[1:])
			return w, h, err == nil
		}
	}
	return 0, 0, false
}

var errBadSPS = errors.New("rtc: malformed SPS")

// bitReader reads an RBSP (emulation prevention bytes removed) MSB first. Reading past the end
// sets err instead of panicking.
type bitReader struct {
	data []byte
	pos  int
	err  error
}

func newBitReader(data []byte) *bitReader {
	return &bitReader{data: bytes.ReplaceAll(data, []byte{0, 0, 3}, []byte{0, 0})}
}

func (b *bitReader) bit() int {
	if b.pos>>3 >= len(b.data) {
		b.err = errBadSPS
		return 0
	}
	v := int(b.data[b.pos>>3]>>(7-b.pos&7)) & 1
	b.pos++
	return v
}

func (b *bitReader) bits(n int) int {
	v := 0
	for range n {
		v = v<<1 | b.bit()
	}
	return v
}

// ue reads an unsigned Exp-Golomb code.
func (b *bitReader) ue() int {
	zeros := 0
	for b.bit() == 0 {
		if b.err != nil {
			return 0
		}
		zeros++
		if zeros > 31 {
			b.err = errBadSPS
			return 0
		}
	}
	return 1<<zeros - 1 + b.bits(zeros)
}

// se reads a signed Exp-Golomb code.
func (b *bitReader) se() int {
	v := b.ue()
	if v&1 != 0 {
		return (v + 1) / 2
	}
	return -(v / 2)
}

// highProfiles carry chroma format and scaling lists in their SPS.
var highProfiles = map[int]bool{100: true, 110: true, 122: true, 244: true, 44: true, 83: true,
	86: true, 118: true, 128: true, 138: true, 139: true, 134: true, 135: true}

// parseSPS reads the cropped picture size from an SPS payload (after the NAL header byte).
func parseSPS(data []byte) (width, height int, err error) {
	b := newBitReader(data)
	profile := b.bits(8)
	b.bits(16) // constraint flags and level
	b.ue()     // seq_parameter_set_id
	chroma := 1
	if highProfiles[profile] {
		chroma = b.ue()
		if chroma == 3 {
			b.bit() // separate_colour_plane_flag
		}
		b.ue()  // bit_depth_luma_minus8
		b.ue()  // bit_depth_chroma_minus8
		b.bit() // qpprime_y_zero_transform_bypass_flag
		if b.bit() == 1 {
			skipScalingLists(b, chroma)
		}
	}
	b.ue()          // log2_max_frame_num_minus4
	switch b.ue() { // pic_order_cnt_type
	case 0:
		b.ue()
	case 1:
		b.bit()
		b.se()
		b.se()
		cycle := b.ue()
		if cycle > 255 {
			return 0, 0, errBadSPS
		}
		for range cycle {
			b.se()
		}
	}
	b.ue()  // max_num_ref_frames
	b.bit() // gaps_in_frame_num_value_allowed_flag
	widthMBs := b.ue() + 1
	heightUnits := b.ue() + 1
	frameMBsOnly := b.bit()
	if frameMBsOnly == 0 {
		b.bit() // mb_adaptive_frame_field_flag
	}
	b.bit() // direct_8x8_inference_flag
	width, height = widthMBs*16, heightUnits*16*(2-frameMBsOnly)
	if b.bit() == 1 { // frame_cropping_flag
		left, right, top, bottom := b.ue(), b.ue(), b.ue(), b.ue()
		cropX, cropY := 1, 2-frameMBsOnly
		if chroma == 1 || chroma == 2 {
			cropX = 2
		}
		if chroma == 1 {
			cropY *= 2
		}
		width -= (left + right) * cropX
		height -= (top + bottom) * cropY
	}
	if b.err != nil {
		return 0, 0, b.err
	}
	return width, height, nil
}

func skipScalingLists(b *bitReader, chroma int) {
	lists := 8
	if chroma == 3 {
		lists = 12
	}
	for i := range lists {
		if b.bit() == 0 {
			continue
		}
		size := 16
		if i >= 6 {
			size = 64
		}
		last, next := 8, 8
		for range size {
			if b.err != nil {
				return
			}
			if next != 0 {
				next = ((last+b.se()+256)%256 + 256) % 256
			}
			if next != 0 {
				last = next
			}
		}
	}
}
