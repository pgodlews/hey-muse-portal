// Tests ported from linux/tests/test_video_call.py in Meta's Muse Gadget SDK, Copyright (c) Meta
// Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0 (see
// ../LICENSE), with changes.

package rtc

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// stunAttrs splits a STUN message's attributes by type.
func stunAttrs(packet []byte) map[uint16][]byte {
	attrs := map[uint16][]byte{}
	for offset := 20; offset+4 <= len(packet); {
		kind := binary.BigEndian.Uint16(packet[offset:])
		size := int(binary.BigEndian.Uint16(packet[offset+2:]))
		attrs[kind] = packet[offset+4 : offset+4+size]
		offset += 4 + size + (4-size%4)%4
	}
	return attrs
}

// stunSuccess is the endpoint's answer to request: a success with MESSAGE-INTEGRITY.
func stunSuccess(request []byte, password string) []byte {
	header := stunHeader(stunBindingSuccess, 24, stunTxID(request[8:20]))
	return stunAttr(header, stunMessageIntegrity, stunMAC(password, header))
}

func TestSTUNCheckIsAuthenticatedAndFingerprinted(t *testing.T) {
	password := "pppppppppppppppppppppppp"
	packet, txid := bindingRequest("server:client", password, 12345, 7, true)
	if !isSTUN(packet) || stunTxID(packet[8:20]) != txid {
		t.Fatal("not a STUN request with its txid")
	}
	if int(binary.BigEndian.Uint16(packet[2:]))+20 != len(packet) {
		t.Fatal("header length")
	}
	attrs := stunAttrs(packet)
	if string(attrs[stunUsername]) != "server:client" {
		t.Fatalf("username %q", attrs[stunUsername])
	}
	if _, ok := attrs[stunUseCandidate]; !ok {
		t.Fatal("no USE-CANDIDATE")
	}
	if binary.BigEndian.Uint32(attrs[stunPriority]) != 12345 || binary.BigEndian.Uint64(attrs[stunIceControlling]) != 7 {
		t.Fatal("priority or tie-breaker")
	}
	crc := crc32.ChecksumIEEE(packet[:len(packet)-8]) ^ stunFingerprintXOR
	if binary.BigEndian.Uint32(attrs[stunFingerprint]) != crc {
		t.Fatal("fingerprint")
	}
	// MESSAGE-INTEGRITY covers everything before it, with the length counting it.
	mi := len(packet) - 8 - 24
	prefix := append([]byte(nil), packet[:mi]...)
	binary.BigEndian.PutUint16(prefix[2:], uint16(mi-20+24))
	if string(stunMAC(password, prefix)) != string(attrs[stunMessageIntegrity]) {
		t.Fatal("message integrity")
	}
	if parseBindingResponse(packet, txid, password) {
		t.Fatal("a request is not a success")
	}
	plain, _ := bindingRequest("a:b", password, 1, 1, false)
	if _, ok := stunAttrs(plain)[stunUseCandidate]; ok {
		t.Fatal("USE-CANDIDATE without nomination")
	}
}

func TestSTUNResponseIsVerified(t *testing.T) {
	password := "pppppppppppppppppppppppp"
	request, txid := bindingRequest("server:client", password, 1, 2, false)
	answer := stunSuccess(request, password)
	if !parseBindingResponse(answer, txid, password) {
		t.Fatal("a valid answer was rejected")
	}
	if parseBindingResponse(answer, txid, "wrong") {
		t.Fatal("accepted the wrong password")
	}
	other, otherTx := bindingRequest("server:client", password, 1, 2, false)
	_ = other
	if parseBindingResponse(answer, otherTx, password) {
		t.Fatal("accepted another transaction's answer")
	}
	if parseBindingResponse(answer[:len(answer)-4], txid, password) {
		t.Fatal("accepted a truncated answer")
	}
	unsigned := stunHeader(stunBindingSuccess, 0, txid)
	if parseBindingResponse(unsigned, txid, password) {
		t.Fatal("accepted an answer without integrity")
	}
}
