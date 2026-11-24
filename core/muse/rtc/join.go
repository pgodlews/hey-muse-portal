// The media-only call JOIN: request encoding and response decoding. This is a modified port of
// linux/src/musegadget/rtc/join.py from Meta's Muse Gadget SDK, Copyright (c) Meta Platforms,
// Inc. and affiliates, licensed under the Apache License, Version 2.0 (see ../LICENSE).
//
// A JOIN is a Thrift Compact message header followed by a message body. The client mints every
// credential: ICE fragments and passwords for both ends, a DTLS identity for each end (required
// fields, unused because the SRTP key travels in the JOIN), and the SRTP master key.

package rtc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Media endpoint types: the VM's vip_type names and their numeric JOIN codes.
const (
	EndpointMetaAI = 2 // "META_AI"
	EndpointHatch  = 8 // "HATCH"
)

// EndpointType maps the VM's vip_type name ("META_AI" or "HATCH") to its JOIN code.
func EndpointType(name string) (int, bool) {
	switch name {
	case "META_AI":
		return EndpointMetaAI, true
	case "HATCH":
		return EndpointHatch, true
	}
	return 0, false
}

// Track and DTLS codes written into the JOIN.
const (
	audioTrackAssistant = 1
	videoTrackAssistant = 2
	videoTrackNone      = 8
	dtlsActive          = 1
)

const iceChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"

// SRTPKeyLen is the length of the SRTP master key (16 bytes) followed by its salt (14 bytes).
const SRTPKeyLen = masterKeyLen + masterSaltLen

// Offer is a built JOIN request plus the credentials it carries, which the media session needs.
// The ICE passwords and SRTP key are secrets: never log them.
type Offer struct {
	Payload         []byte // the Thrift message to send (base64) as payload_b64
	ClientUfrag     string // our ICE username fragment
	ServerUfrag     string // the endpoint's proposed ufrag (the answer may replace it)
	ClientPwd       string // our ICE password
	ServerPwd       string // the endpoint's ICE password: keys our connectivity checks
	SRTPKey         []byte // 30-byte SRTP master key+salt, used in both directions
	ClientSessionID string // 10 decimal digits
	TransactionID   int32
}

// Answer holds the JOIN response fields the media session needs.
type Answer struct {
	ServerUfrag string // the endpoint's ICE ufrag; checks use "ServerUfrag:ClientUfrag"
	Status      int    // the header's status code (200), or 0 when absent
	IPv4        string // the endpoint's address, if it named one; otherwise use the edge host
	IPv6        string
}

// BuildJoin builds a media-only JOIN for the endpoint at edgeHost:edgePort. threadID may be empty.
// vipType is EndpointMetaAI or EndpointHatch; video asks for Muse's video track.
func BuildJoin(threadID, edgeHost string, edgePort int, vipType int, video bool) (*Offer, error) {
	o := &Offer{
		ClientUfrag: iceToken(4),
		ServerUfrag: iceToken(16),
		ClientPwd:   iceToken(24),
		ServerPwd:   iceToken(24),
		SRTPKey:     make([]byte, SRTPKeyLen),
	}
	rand.Read(o.SRTPKey)
	_, _, clientFingerprint, err := dtlsIdentity()
	if err != nil {
		return nil, err
	}
	serverKeyPEM, serverCertPEM, _, err := dtlsIdentity()
	if err != nil {
		return nil, err
	}
	o.ClientSessionID = strconv.FormatInt(randBelow(9_000_000_000)+1_000_000_000, 10)
	o.TransactionID = int32(randBelow(1<<31-1) + 1)

	w := newThriftWriter()
	w.str(1, o.ClientSessionID)
	w.i32(3, o.TransactionID)
	w.stop()
	w.begin(1) // body.joinRequest
	w.str(1, o.ClientUfrag)
	w.str(2, o.ServerUfrag)
	w.begin(3)
	w.str(1, base64.StdEncoding.EncodeToString(o.SRTPKey))
	w.str(2, o.ClientPwd)
	w.str(3, o.ServerPwd)
	w.binary(4, serverKeyPEM)
	w.end()
	w.str(4, clientFingerprint)
	w.binary(5, serverCertPEM)
	w.str(6, edgeHost+":"+strconv.Itoa(edgePort))
	w.i32(8, dtlsActive)
	w.i32(11, audioTrackAssistant)
	if video {
		w.i32(12, videoTrackAssistant)
	} else {
		w.i32(12, videoTrackNone)
	}
	w.i32(13, int32(vipType))
	w.boolean(14, false)
	w.binary(16, sessionParams(threadID))
	w.end()
	w.stop()
	o.Payload = w.out
	return o, nil
}

// sessionParams encodes the call's session parameters: use case, callee, entry point and thread.
func sessionParams(threadID string) []byte {
	w := newThriftWriter()
	w.i32(1, 0)
	w.str(2, "")
	w.str(3, "")
	if threadID != "" {
		w.str(4, threadID)
	}
	w.stop()
	return w.out
}

// ParseJoinResponse decodes the JOIN response payload (after base64 decoding).
func ParseJoinResponse(b []byte) (*Answer, error) {
	if len(b) > maxResponseBytes {
		return nil, errors.New("rtc: JOIN response is too large")
	}
	r := &thriftReader{data: b}
	header, err := r.readStruct()
	if err != nil {
		return nil, err
	}
	body, err := r.readStruct()
	if err != nil {
		return nil, err
	}
	answer, ok := body[2].(thriftStruct)
	if !ok {
		return nil, errors.New("rtc: JOIN response carried no joinResponse")
	}
	a := &Answer{}
	fields := []*string{&a.ServerUfrag, &a.IPv4, &a.IPv6}
	for i, dst := range fields {
		v, present := answer[i+1]
		raw, isBytes := v.([]byte)
		if present && !isBytes {
			return nil, errors.New("rtc: malformed joinResponse")
		}
		*dst = string(raw)
	}
	if a.ServerUfrag == "" {
		return nil, errors.New("rtc: JOIN response carried no server ufrag")
	}
	if status, ok := header[4].(int64); ok {
		if status != 200 {
			return nil, fmt.Errorf("rtc: JOIN refused (%d)", status)
		}
		a.Status = int(status)
	}
	return a, nil
}

// iceToken returns n random characters from the ICE character set.
func iceToken(n int) string {
	var sb strings.Builder
	for range n {
		sb.WriteByte(iceChars[randBelow(int64(len(iceChars)))])
	}
	return sb.String()
}

// randBelow returns a uniform random integer in [0, n).
func randBelow(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return v.Int64()
}

// dtlsIdentity mints a self-signed P-256 identity: the private key (PKCS#8 PEM), the certificate
// (PEM) and its fingerprint as "sha-256 AA:BB:...".
func dtlsIdentity() (keyPEM, certPEM []byte, fingerprint string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 159))
	if err != nil {
		return nil, nil, "", err
	}
	name := pkix.Name{CommonName: "musegadget"}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:       serial.Add(serial, big.NewInt(1)),
		Subject:            name,
		Issuer:             name,
		NotBefore:          now.Add(-24 * time.Hour),
		NotAfter:           now.Add(30 * 24 * time.Hour),
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, "", err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return keyPEM, certPEM, certFingerprint(der), nil
}

// certFingerprint formats a certificate's SHA-256 digest the way SDP does.
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	digest := strings.ToUpper(hex.EncodeToString(sum[:]))
	pairs := make([]string, 0, len(sum))
	for i := 0; i < len(digest); i += 2 {
		pairs = append(pairs, digest[i:i+2])
	}
	return "sha-256 " + strings.Join(pairs, ":")
}
