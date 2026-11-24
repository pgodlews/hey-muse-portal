// SRTP and SRTCP with AES_CM_128_HMAC_SHA1_80 (RFC 3711), keyed directly. This is a modified port
// of linux/src/musegadget/rtc/srtp.py from Meta's Muse Gadget SDK, Copyright (c) Meta Platforms,
// Inc. and affiliates, licensed under the Apache License, Version 2.0 (see ../LICENSE).
//
// The call's JOIN carries one 30-byte master key+salt, used for both directions; streams are told
// apart by SSRC.

package rtc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"hash"
)

const (
	srtpTagLen    = 10
	masterKeyLen  = 16
	masterSaltLen = 14
	// srtpMaxStreams bounds the per-SSRC state an untrusted peer can make us keep.
	srtpMaxStreams = 16
	replayWindow   = 64
)

// SRTP errors. Packets that fail are dropped by the media session and counted in Stats.
var (
	ErrSRTPAuth    = errors.New("rtc: SRTP authentication failed")
	ErrSRTPReplay  = errors.New("rtc: replayed or stale SRTP packet")
	ErrSRTPShort   = errors.New("rtc: short SRTP packet")
	ErrSRTPStreams = errors.New("rtc: too many SRTP streams")
)

// keystream returns length bytes of AES-CTR output for the 16-byte counter block iv.
func keystream(block cipher.Block, iv [16]byte, length int) []byte {
	out := make([]byte, length)
	cipher.NewCTR(block, iv[:]).XORKeyStream(out, out)
	return out
}

// xorKeystream encrypts or decrypts data in place.
func xorKeystream(block cipher.Block, iv [16]byte, data []byte) {
	cipher.NewCTR(block, iv[:]).XORKeyStream(data, data)
}

// deriveKey is the RFC 3711 key derivation (key derivation rate 0): AES-CM over the master salt
// with the label XORed in above the 48-bit index.
func deriveKey(masterKey, masterSalt []byte, label byte, length int) []byte {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		panic(err) // the key length is checked by the caller
	}
	var iv [16]byte
	copy(iv[:], masterSalt)
	iv[7] ^= label
	return keystream(block, iv, length)
}

// srtpKeys are the session keys for one of SRTP or SRTCP.
type srtpKeys struct {
	enc  cipher.Block
	auth []byte
	salt [masterSaltLen]byte
}

func newSRTPKeys(masterKey, masterSalt []byte, baseLabel byte) *srtpKeys {
	enc, err := aes.NewCipher(deriveKey(masterKey, masterSalt, baseLabel, 16))
	if err != nil {
		panic(err)
	}
	k := &srtpKeys{enc: enc, auth: deriveKey(masterKey, masterSalt, baseLabel+1, 20)}
	copy(k.salt[:], deriveKey(masterKey, masterSalt, baseLabel+2, masterSaltLen))
	return k
}

// iv is (salt << 16) XOR (ssrc << 64) XOR (index << 16), as a 128-bit counter block.
func (k *srtpKeys) iv(ssrc uint32, index uint64) [16]byte {
	var iv [16]byte
	copy(iv[:], k.salt[:])
	var s [4]byte
	binary.BigEndian.PutUint32(s[:], ssrc)
	for i := range 4 {
		iv[4+i] ^= s[i]
	}
	for i := range 6 { // the 48-bit index lands in bytes 8..13
		iv[13-i] ^= byte(index >> (8 * i))
	}
	return iv
}

func (k *srtpKeys) tag(parts ...[]byte) []byte {
	var mac hash.Hash = hmac.New(sha1.New, k.auth)
	for _, p := range parts {
		mac.Write(p)
	}
	return mac.Sum(nil)[:srtpTagLen]
}

// rtpHeaderLen returns the length of an RTP header including CSRCs and any extension.
func rtpHeaderLen(packet []byte) (int, error) {
	if len(packet) < 12 {
		return 0, errors.New("rtc: short RTP packet")
	}
	length := 12 + 4*int(packet[0]&0x0F)
	if packet[0]&0x10 != 0 {
		if len(packet) < length+4 {
			return 0, errors.New("rtc: short RTP extension")
		}
		length += 4 + 4*int(binary.BigEndian.Uint16(packet[length+2:]))
	}
	if len(packet) < length {
		return 0, errors.New("rtc: short RTP header")
	}
	return length, nil
}

// replayState is the RFC 3711 3.3.2 window: it rejects packets already seen or older than 64
// behind the newest.
type replayState struct {
	started bool
	top     uint64
	seen    uint64
}

func (w *replayState) check(index uint64) error {
	switch {
	case !w.started:
		w.started, w.top, w.seen = true, index, 1
	case index > w.top:
		if shift := index - w.top; shift < replayWindow {
			w.seen = w.seen<<shift | 1
		} else {
			w.seen = 1
		}
		w.top = index
	default:
		behind := w.top - index
		if behind >= replayWindow || w.seen>>behind&1 != 0 {
			return ErrSRTPReplay
		}
		w.seen |= 1 << behind
	}
	return nil
}

func windowFor(windows map[uint32]*replayState, ssrc uint32) (*replayState, error) {
	w, ok := windows[ssrc]
	if !ok {
		if len(windows) >= srtpMaxStreams {
			return nil, ErrSRTPStreams
		}
		w = &replayState{}
		windows[ssrc] = w
	}
	return w, nil
}

// rocState is a stream's rollover counter and the last sequence number that set it.
type rocState struct {
	roc  uint32
	last uint16
}

// srtpSession is one direction-agnostic SRTP context. It is not safe for concurrent use.
type srtpSession struct {
	rtp, rtcp     *srtpKeys
	txROC, rxROC  map[uint32]rocState
	txRTCPIndex   map[uint32]uint32
	rxRTPWindows  map[uint32]*replayState
	rxRTCPWindows map[uint32]*replayState
}

func newSRTPSession(keySalt []byte) (*srtpSession, error) {
	if len(keySalt) != SRTPKeyLen {
		return nil, errors.New("rtc: SRTP master key+salt must be 30 bytes")
	}
	key, salt := keySalt[:masterKeyLen], keySalt[masterKeyLen:]
	return &srtpSession{
		rtp:           newSRTPKeys(key, salt, 0),
		rtcp:          newSRTPKeys(key, salt, 3),
		txROC:         map[uint32]rocState{},
		rxROC:         map[uint32]rocState{},
		txRTCPIndex:   map[uint32]uint32{},
		rxRTPWindows:  map[uint32]*replayState{},
		rxRTCPWindows: map[uint32]*replayState{},
	}, nil
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// protectRTP encrypts an RTP packet and appends its tag. The input is not modified.
func (s *srtpSession) protectRTP(packet []byte) ([]byte, error) {
	headerLen, err := rtpHeaderLen(packet)
	if err != nil {
		return nil, err
	}
	ssrc := binary.BigEndian.Uint32(packet[8:])
	seq := binary.BigEndian.Uint16(packet[2:])
	st, ok := s.txROC[ssrc]
	if !ok {
		st = rocState{0, seq}
	}
	if seq < st.last && st.last-seq > 0x8000 {
		st.roc++
	}
	s.txROC[ssrc] = rocState{st.roc, seq}
	index := uint64(st.roc)<<16 | uint64(seq)
	out := make([]byte, len(packet), len(packet)+srtpTagLen)
	copy(out, packet)
	xorKeystream(s.rtp.enc, s.rtp.iv(ssrc, index), out[headerLen:])
	return append(out, s.rtp.tag(out, be32(st.roc))...), nil
}

// unprotectRTP authenticates and decrypts an SRTP packet into a new slice.
func (s *srtpSession) unprotectRTP(packet []byte) ([]byte, error) {
	if len(packet) < 12+srtpTagLen {
		return nil, ErrSRTPShort
	}
	body, tag := packet[:len(packet)-srtpTagLen], packet[len(packet)-srtpTagLen:]
	headerLen, err := rtpHeaderLen(body)
	if err != nil {
		return nil, err
	}
	ssrc := binary.BigEndian.Uint32(body[8:])
	seq := binary.BigEndian.Uint16(body[2:])
	st, known := s.rxROC[ssrc]
	if !known {
		st = rocState{0, seq}
	}
	guess := estimateROC(st, seq)
	if !hmac.Equal(s.rtp.tag(body, be32(guess)), tag) {
		return nil, ErrSRTPAuth
	}
	w, err := windowFor(s.rxRTPWindows, ssrc)
	if err != nil {
		return nil, err
	}
	index := uint64(guess)<<16 | uint64(seq)
	if err := w.check(index); err != nil {
		return nil, err
	}
	if (guess == st.roc && seq > st.last) || guess == st.roc+1 || !known {
		s.rxROC[ssrc] = rocState{guess, seq}
	}
	out := append([]byte(nil), body...)
	xorKeystream(s.rtp.enc, s.rtp.iv(ssrc, index), out[headerLen:])
	return out, nil
}

// estimateROC is RFC 3711 3.3.1: guess the sender's rollover counter for seq.
func estimateROC(st rocState, seq uint16) uint32 {
	last, s := int(st.last), int(seq)
	if last < 0x8000 {
		if s-last > 0x8000 {
			return st.roc - 1
		}
	} else if last-0x8000 > s {
		return st.roc + 1
	}
	return st.roc
}

// protectRTCP encrypts an RTCP compound packet, appends E|index and the tag.
func (s *srtpSession) protectRTCP(packet []byte) ([]byte, error) {
	if len(packet) < 8 {
		return nil, errors.New("rtc: short RTCP packet")
	}
	ssrc := binary.BigEndian.Uint32(packet[4:])
	// Like libsrtp, the first SRTCP index sent is 1.
	index := (s.txRTCPIndex[ssrc] + 1) & 0x7FFFFFFF
	s.txRTCPIndex[ssrc] = index
	out := make([]byte, len(packet), len(packet)+4+srtpTagLen)
	copy(out, packet)
	xorKeystream(s.rtcp.enc, s.rtcp.iv(ssrc, uint64(index)), out[8:])
	out = binary.BigEndian.AppendUint32(out, 0x80000000|index)
	return append(out, s.rtcp.tag(out)...), nil
}

// unprotectRTCP authenticates and decrypts an SRTCP packet into a new slice.
func (s *srtpSession) unprotectRTCP(packet []byte) ([]byte, error) {
	if len(packet) < 8+4+srtpTagLen {
		return nil, ErrSRTPShort
	}
	body, tag := packet[:len(packet)-srtpTagLen], packet[len(packet)-srtpTagLen:]
	if !hmac.Equal(s.rtcp.tag(body), tag) {
		return nil, ErrSRTPAuth
	}
	eIndex := binary.BigEndian.Uint32(body[len(body)-4:])
	body = body[:len(body)-4]
	index := eIndex & 0x7FFFFFFF
	ssrc := binary.BigEndian.Uint32(body[4:])
	w, err := windowFor(s.rxRTCPWindows, ssrc)
	if err != nil {
		return nil, err
	}
	if err := w.check(uint64(index)); err != nil {
		return nil, err
	}
	out := append([]byte(nil), body...)
	if eIndex&0x80000000 != 0 {
		xorKeystream(s.rtcp.enc, s.rtcp.iv(ssrc, uint64(index)), out[8:])
	}
	return out, nil
}
