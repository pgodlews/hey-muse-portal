// Minimal STUN for ICE-lite connectivity checks and consent (RFC 5389, RFC 8445). This is a
// modified port of linux/src/musegadget/rtc/stun.py from Meta's Muse Gadget SDK, Copyright (c)
// Meta Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0 (see
// ../LICENSE).

package rtc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"hash/crc32"
)

const (
	stunMagic          = 0x2112A442
	stunBindingRequest = 0x0001
	stunBindingSuccess = 0x0101

	stunUsername         = 0x0006
	stunMessageIntegrity = 0x0008
	stunPriority         = 0x0024
	stunUseCandidate     = 0x0025
	stunFingerprint      = 0x8028
	stunIceControlling   = 0x802A

	stunFingerprintXOR = 0x5354554E
)

// stunTxID is a STUN transaction id.
type stunTxID [12]byte

// isSTUN reports whether a packet on the media path is STUN rather than RTP or RTCP.
func isSTUN(data []byte) bool {
	return len(data) >= 20 && data[0] < 2 && binary.BigEndian.Uint32(data[4:]) == stunMagic
}

func stunAttr(dst []byte, kind uint16, value []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, kind)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(value)))
	dst = append(dst, value...)
	return append(dst, make([]byte, (4-len(value)%4)%4)...)
}

func stunHeader(kind uint16, length int, txid stunTxID) []byte {
	h := binary.BigEndian.AppendUint16(make([]byte, 0, 20), kind)
	h = binary.BigEndian.AppendUint16(h, uint16(length))
	h = binary.BigEndian.AppendUint32(h, stunMagic)
	return append(h, txid[:]...)
}

func stunMAC(password string, data []byte) []byte {
	mac := hmac.New(sha1.New, []byte(password))
	mac.Write(data)
	return mac.Sum(nil)
}

// bindingRequest builds a controlling connectivity check, nominating the path if asked. It is
// authenticated with the endpoint's ICE password and fingerprinted.
func bindingRequest(username, password string, priority uint32, tieBreaker uint64, nominate bool) ([]byte, stunTxID) {
	var txid stunTxID
	rand.Read(txid[:])
	attrs := stunAttr(nil, stunUsername, []byte(username))
	attrs = stunAttr(attrs, stunPriority, binary.BigEndian.AppendUint32(nil, priority))
	attrs = stunAttr(attrs, stunIceControlling, binary.BigEndian.AppendUint64(nil, tieBreaker))
	if nominate {
		attrs = stunAttr(attrs, stunUseCandidate, nil)
	}
	// The integrity covers a header whose length already counts the MESSAGE-INTEGRITY attribute.
	header := stunHeader(stunBindingRequest, len(attrs)+24, txid)
	attrs = stunAttr(attrs, stunMessageIntegrity, stunMAC(password, append(header, attrs...)))
	// The fingerprint likewise covers a header that counts the FINGERPRINT attribute.
	header = stunHeader(stunBindingRequest, len(attrs)+8, txid)
	crc := crc32.ChecksumIEEE(append(header, attrs...)) ^ stunFingerprintXOR
	attrs = stunAttr(attrs, stunFingerprint, binary.BigEndian.AppendUint32(nil, crc))
	return append(header, attrs...), txid
}

// parseBindingResponse reports whether data is an authenticated success response to txid.
func parseBindingResponse(data []byte, txid stunTxID, password string) bool {
	if !isSTUN(data) || stunTxID(data[8:20]) != txid {
		return false
	}
	kind := binary.BigEndian.Uint16(data[0:])
	length := int(binary.BigEndian.Uint16(data[2:]))
	if kind != stunBindingSuccess || len(data) < 20+length {
		return false
	}
	for offset := 20; offset+4 <= 20+length; {
		attr := binary.BigEndian.Uint16(data[offset:])
		size := int(binary.BigEndian.Uint16(data[offset+2:]))
		if attr == stunMessageIntegrity {
			if offset+24 > len(data) {
				return false
			}
			prefix := append([]byte(nil), data[:offset]...)
			binary.BigEndian.PutUint16(prefix[2:], uint16(offset-20+24))
			return hmac.Equal(stunMAC(password, prefix), data[offset+4:offset+24])
		}
		offset += 4 + size + (4-size%4)%4
	}
	return false
}
