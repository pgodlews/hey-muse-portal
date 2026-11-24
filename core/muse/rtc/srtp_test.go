// Tests ported from linux/tests/test_rtc_srtp.py in Meta's Muse Gadget SDK, Copyright (c) Meta
// Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0 (see
// ../LICENSE), with changes. The libsrtp interop tests there need pylibsrtp; the known answers
// they produced are kept below.

package rtc

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testRTP(seq uint16, ssrc uint32, ext bool) []byte {
	first := byte(0x80)
	if ext {
		first = 0x90
	}
	p := []byte{first, 127}
	p = binary.BigEndian.AppendUint16(p, seq)
	p = binary.BigEndian.AppendUint32(p, uint32(seq)*3000)
	p = binary.BigEndian.AppendUint32(p, ssrc)
	if ext {
		p = append(p, 0xBE, 0xDE, 0x00, 0x01, 0x31, 0x00, 0x01, 0x00)
	}
	return append(p, bytes.Repeat([]byte("x"), 100)...)
}

func countingKey() []byte {
	k := make([]byte, 30)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func mustSession(t *testing.T, key []byte) *srtpSession {
	t.Helper()
	s, err := newSRTPSession(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// RFC 3711 appendix B.3.
func TestRFC3711KeyDerivationVector(t *testing.T) {
	mk := unhex(t, "E1F97A0D3E018BE0D64FA32C06DE4139")
	ms := unhex(t, "0EC675AD498AFEEBB6960B3AABE6")
	cases := []struct {
		label  byte
		length int
		want   string
	}{
		{0, 16, "C61E7A93744F39EE10734AFE3FF7A087"},
		{1, 20, "CEBE321F6FF7716B6FD4AB49AF256A156D38BAA4"},
		{2, 14, "30CBBC08863D8C85D49DB34A9AE1"},
	}
	for _, c := range cases {
		if got := strings.ToUpper(hex.EncodeToString(deriveKey(mk, ms, c.label, c.length))); got != c.want {
			t.Errorf("label %d: %s, want %s", c.label, got, c.want)
		}
	}
}

// RFC 3711 appendix B.2: the AES-CM keystream for SSRC 0, index 0.
func TestRFC3711KeystreamVector(t *testing.T) {
	block, _ := aes.NewCipher(unhex(t, "2B7E151628AED2A6ABF7158809CF4F3C"))
	keys := &srtpKeys{enc: block}
	copy(keys.salt[:], unhex(t, "F0F1F2F3F4F5F6F7F8F9FAFBFCFD"))
	got := keystream(block, keys.iv(0, 0), 48)
	want := unhex(t, "E03EAD0935C95E80E166B16DD92B4EB4 D23513162B02D0F72A43A2FE4A5F97AB 41E95B3BB0A2E8DD477901E4FCA894C0")
	if !bytes.Equal(got, want) {
		t.Fatalf("keystream %x", got)
	}
}

func TestKnownAnswersFromLibsrtp(t *testing.T) {
	ours := mustSession(t, countingKey())
	packet := buildRTP(111, 4660, 960*7, 100, []byte("opus-payload-0123456789"), false)
	protected := unhex(t, "806f123400001a40000000649c5fa8537a5563ed445b2da0c83e0b8d1fa94d9f7f3e264882c67696106f276ab8")
	got, err := ours.protectRTP(packet)
	if err != nil || !bytes.Equal(got, protected) {
		t.Fatalf("protect: %x %v", got, err)
	}
	plain, err := mustSession(t, countingKey()).unprotectRTP(protected)
	if err != nil || !bytes.Equal(plain, packet) {
		t.Fatalf("unprotect: %x %v", plain, err)
	}
	pli, err := ours.protectRTCP(buildPLI(101, 201))
	if err != nil || !bytes.Equal(pli, unhex(t, "81ce000200000065270b4d4180000001179c9c93e63b478aa918")) {
		t.Fatalf("protect RTCP: %x %v", pli, err)
	}
}

func TestRTPRoundTripAcrossRollover(t *testing.T) {
	for _, ext := range []bool{false, true} {
		key := make([]byte, 30)
		rand.Read(key)
		tx, rx := mustSession(t, key), mustSession(t, key)
		for _, seq := range []uint16{65530, 65531, 65535, 0, 1, 2} {
			packet := testRTP(seq, 201, ext)
			protected, err := tx.protectRTP(packet)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(protected[12:len(packet)], packet[12:]) {
				t.Fatal("payload not encrypted")
			}
			plain, err := rx.unprotectRTP(protected)
			if err != nil || !bytes.Equal(plain, packet) {
				t.Fatalf("ext=%v seq=%d: %v", ext, seq, err)
			}
		}
		if tx.txROC[201].roc != 1 || rx.rxROC[201].roc != 1 {
			t.Fatalf("rollover counters %d %d", tx.txROC[201].roc, rx.rxROC[201].roc)
		}
	}
}

func TestRTCPRoundTrip(t *testing.T) {
	key := make([]byte, 30)
	rand.Read(key)
	tx, rx := mustSession(t, key), mustSession(t, key)
	sr := []byte{0x80, 200, 0, 6, 0, 0, 0, 201}
	sr = append(sr, bytes.Repeat([]byte{7}, 20)...)
	protected, err := tx.protectRTCP(sr)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := rx.unprotectRTCP(protected)
	if err != nil || !bytes.Equal(plain, sr) {
		t.Fatalf("%x %v", plain, err)
	}
}

func TestTamperingIsRejected(t *testing.T) {
	key := make([]byte, 30)
	rand.Read(key)
	s := mustSession(t, key)
	protected, _ := s.protectRTP(testRTP(7, 201, false))
	protected[20] ^= 1
	if _, err := s.unprotectRTP(protected); !errors.Is(err, ErrSRTPAuth) {
		t.Fatalf("tampered packet: %v", err)
	}
}

func TestReplayedAndStalePacketsAreRejected(t *testing.T) {
	tx, rx := mustSession(t, countingKey()), mustSession(t, countingKey())
	first, _ := tx.protectRTP(testRTP(10, 201, false))
	if _, err := rx.unprotectRTP(first); err != nil {
		t.Fatal(err)
	}
	if _, err := rx.unprotectRTP(first); !errors.Is(err, ErrSRTPReplay) {
		t.Fatalf("replay: %v", err)
	}
	var late [][]byte
	for seq := uint16(11); seq < 100; seq++ {
		p, _ := tx.protectRTP(testRTP(seq, 201, false))
		late = append(late, p)
	}
	if _, err := rx.unprotectRTP(late[len(late)-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := rx.unprotectRTP(late[len(late)-2]); err != nil { // reordered but inside the window
		t.Fatal(err)
	}
	if _, err := rx.unprotectRTP(late[0]); !errors.Is(err, ErrSRTPReplay) { // 88 behind the newest
		t.Fatalf("stale: %v", err)
	}
	sent, _ := tx.protectRTCP(buildPLI(101, 201))
	if _, err := rx.unprotectRTCP(sent); err != nil {
		t.Fatal(err)
	}
	if _, err := rx.unprotectRTCP(sent); !errors.Is(err, ErrSRTPReplay) {
		t.Fatalf("RTCP replay: %v", err)
	}
}

func TestStreamCountIsBounded(t *testing.T) {
	tx, rx := mustSession(t, countingKey()), mustSession(t, countingKey())
	for ssrc := range uint32(srtpMaxStreams) {
		p, _ := tx.protectRTP(testRTP(1, ssrc, false))
		if _, err := rx.unprotectRTP(p); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := tx.protectRTP(testRTP(1, srtpMaxStreams, false))
	if _, err := rx.unprotectRTP(p); !errors.Is(err, ErrSRTPStreams) {
		t.Fatalf("stream %d: %v", srtpMaxStreams, err)
	}
}

func TestShortPacketsAreErrors(t *testing.T) {
	s := mustSession(t, countingKey())
	if _, err := s.unprotectRTP(make([]byte, 21)); err == nil {
		t.Fatal("short SRTP")
	}
	if _, err := s.unprotectRTCP(make([]byte, 21)); err == nil {
		t.Fatal("short SRTCP")
	}
	if _, err := s.protectRTP([]byte{0x90, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("RTP with a missing extension")
	}
	if _, err := newSRTPSession(make([]byte, 29)); err == nil {
		t.Fatal("29-byte key")
	}
}
