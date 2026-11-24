// Tests ported from linux/tests/test_video_call.py in Meta's Muse Gadget SDK, Copyright (c) Meta
// Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0 (see
// ../LICENSE), with changes.

package rtc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
)

const testThread = "f823e827-1022-40ea-b986-1209b491b9e8"

// offerFields is what the fake edge reads from a JOIN, as the SDK's offer_fields does.
type offerFields struct {
	session     []byte
	transaction int64
	clientUfrag string
	srtpKey     []byte
	serverPwd   string
	join        thriftStruct
}

func decodeOffer(t *testing.T, payload []byte) (thriftStruct, thriftStruct, *thriftReader) {
	t.Helper()
	r := &thriftReader{data: payload}
	header, err := r.readStruct()
	if err != nil {
		t.Fatal(err)
	}
	body, err := r.readStruct()
	if err != nil {
		t.Fatal(err)
	}
	return header, body, r
}

func readOffer(t *testing.T, payload []byte) offerFields {
	t.Helper()
	header, body, _ := decodeOffer(t, payload)
	join := body[1].(thriftStruct)
	secrets := join[3].(thriftStruct)
	key, err := base64.StdEncoding.DecodeString(string(secrets[1].([]byte)))
	if err != nil {
		t.Fatal(err)
	}
	return offerFields{
		session:     header[1].([]byte),
		transaction: header[3].(int64),
		clientUfrag: string(join[1].([]byte)),
		srtpKey:     key,
		serverPwd:   string(secrets[3].([]byte)),
		join:        join,
	}
}

// joinResponse encodes a JOIN response the way the SDK's fake edge does.
func joinResponse(o offerFields, serverUfrag string) []byte {
	w := newThriftWriter()
	w.binary(1, o.session)
	w.i32(3, int32(o.transaction))
	w.i32(4, 200)
	w.stop()
	w.begin(2)
	w.str(1, serverUfrag)
	w.str(2, "")
	w.str(3, "")
	w.end()
	w.stop()
	return w.out
}

func mustJoin(t *testing.T, thread string, video bool, vip int) *Offer {
	t.Helper()
	o, err := BuildJoin(thread, "media.example", 3478, vip, video)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestJoinOfferHasTheFieldsTheServerRequires(t *testing.T) {
	offer := mustJoin(t, testThread, true, EndpointMetaAI)
	header, body, r := decodeOffer(t, offer.Payload)
	if r.pos != len(offer.Payload) {
		t.Fatalf("trailing bytes after the JOIN: %d of %d read", r.pos, len(offer.Payload))
	}
	session := string(header[1].([]byte))
	if session != offer.ClientSessionID || len(session) != 10 || strings.Trim(session, "0123456789") != "" {
		t.Fatalf("client session id %q", session)
	}
	if header[3].(int64) != int64(offer.TransactionID) || offer.TransactionID < 1 {
		t.Fatalf("transaction id %v", header[3])
	}
	join := body[1].(thriftStruct)
	if string(join[1].([]byte)) != offer.ClientUfrag || string(join[2].([]byte)) != offer.ServerUfrag {
		t.Fatal("ufrags differ from the offer")
	}
	if len(offer.ClientUfrag) != 4 || len(offer.ServerUfrag) != 16 {
		t.Fatal("ufrag lengths")
	}
	secrets := join[3].(thriftStruct)
	key, _ := base64.StdEncoding.DecodeString(string(secrets[1].([]byte)))
	if !bytes.Equal(key, offer.SRTPKey) || len(key) != 30 {
		t.Fatal("SRTP key differs from the offer")
	}
	if string(secrets[2].([]byte)) != offer.ClientPwd || string(secrets[3].([]byte)) != offer.ServerPwd {
		t.Fatal("ICE passwords differ from the offer")
	}
	if len(offer.ClientPwd) < 22 || len(offer.ServerPwd) < 22 {
		t.Fatal("ICE passwords too short")
	}

	// The server's private key and certificate belong together.
	keyBlock, _ := pem.Decode(secrets[4].([]byte))
	certBlock, _ := pem.Decode(join[5].([]byte))
	if keyBlock == nil || certBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		t.Fatal("server identity is not PEM")
	}
	priv, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !priv.(*ecdsa.PrivateKey).PublicKey.Equal(cert.PublicKey) {
		t.Fatal("server certificate does not match its key")
	}
	if cert.Subject.CommonName != "musegadget" || cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) != nil {
		t.Fatal("server certificate is not a self-signed musegadget certificate")
	}

	fingerprint := string(join[4].([]byte))
	if !strings.HasPrefix(fingerprint, "sha-256 ") || len(strings.Split(strings.Fields(fingerprint)[1], ":")) != 32 {
		t.Fatalf("client fingerprint %q", fingerprint)
	}
	if string(join[6].([]byte)) != "media.example:3478" {
		t.Fatalf("edge %q", join[6])
	}
	if _, ok := join[7]; ok {
		t.Fatal("session parameters ride only in field 16")
	}
	params, err := (&thriftReader{data: join[16].([]byte)}).readStruct()
	if err != nil {
		t.Fatal(err)
	}
	if params[1] != int64(0) || len(params[2].([]byte)) != 0 || len(params[3].([]byte)) != 0 ||
		string(params[4].([]byte)) != testThread || len(params) != 4 {
		t.Fatalf("session params %v", params)
	}
	if join[8] != int64(1) || join[11] != int64(1) || join[12] != int64(2) || join[13] != int64(2) || join[14] != false {
		t.Fatalf("codes %v %v %v %v %v", join[8], join[11], join[12], join[13], join[14])
	}
}

func TestJoinOfferWithoutThreadAndAudioOnly(t *testing.T) {
	offer := mustJoin(t, "", false, EndpointHatch)
	_, body, _ := decodeOffer(t, offer.Payload)
	join := body[1].(thriftStruct)
	params, err := (&thriftReader{data: join[16].([]byte)}).readStruct()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := params[4]; ok {
		t.Fatal("thread present without a thread id")
	}
	if join[12] != int64(8) || join[13] != int64(8) {
		t.Fatalf("codes %v %v", join[12], join[13])
	}
}

func TestEndpointType(t *testing.T) {
	if v, ok := EndpointType("META_AI"); !ok || v != EndpointMetaAI {
		t.Fatal("META_AI")
	}
	if v, ok := EndpointType("HATCH"); !ok || v != EndpointHatch {
		t.Fatal("HATCH")
	}
	if _, ok := EndpointType("OTHER"); ok {
		t.Fatal("OTHER")
	}
}

func TestJoinResponseIsDecoded(t *testing.T) {
	offer := readOffer(t, mustJoin(t, testThread, true, EndpointMetaAI).Payload)
	answer, err := ParseJoinResponse(joinResponse(offer, "updatedServerUfrag1"))
	if err != nil {
		t.Fatal(err)
	}
	if *answer != (Answer{ServerUfrag: "updatedServerUfrag1", Status: 200}) {
		t.Fatalf("answer %+v", answer)
	}
	if _, err := ParseJoinResponse([]byte{0, 0}); err == nil {
		t.Fatal("a response without joinResponse was accepted")
	}
}

func TestJoinResponseParserRejectsOversizedContainers(t *testing.T) {
	// A list header claiming 2**31 booleans, and a string longer than its message.
	for _, payload := range [][]byte{
		{0x00, 0x19, 0xF1, 0xFF, 0xFF, 0xFF, 0xFF, 0x07, 0x00},
		append([]byte{0x18, 0xFF, 0x7F}, []byte("xxxx\x00")...),
		make([]byte, 17*1024),
	} {
		if _, err := ParseJoinResponse(payload); err == nil {
			t.Fatalf("accepted % x", payload[:min(len(payload), 16)])
		}
	}
}

func TestRefusedJoinFailsFast(t *testing.T) {
	offer := readOffer(t, mustJoin(t, testThread, true, EndpointMetaAI).Payload)
	accepted := joinResponse(offer, "u")
	status200 := []byte{0x15, 0x90, 0x03} // header field 4, zigzag varint of 200
	if !bytes.Contains(accepted, status200) {
		t.Fatal("status 200 not where expected")
	}
	refused := bytes.Replace(accepted, status200, []byte{0x15, 0xA0, 0x06}, 1) // 400
	if _, err := ParseJoinResponse(refused); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("refused JOIN: %v", err)
	}
}

func TestThriftRoundTripsEveryKind(t *testing.T) {
	w := newThriftWriter()
	w.i32(1, -5)
	w.boolean(2, true)
	w.boolean(40, false) // a long-form field header
	w.begin(41)
	w.str(1, "nested")
	w.end()
	w.i32(-3, 2147483647) // a negative id is long-form too
	w.stop()
	got, err := (&thriftReader{data: w.out}).readStruct()
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != int64(-5) || got[2] != true || got[40] != false || got[-3] != int64(2147483647) ||
		string(got[41].(thriftStruct)[1].([]byte)) != "nested" {
		t.Fatalf("round trip %v", got)
	}
}

func TestThriftReaderDecodesContainersAndBoundsDepth(t *testing.T) {
	// Field 1: list of 2 i32 (1, -1); field 2: map of 1 binary->bool; field 3: a double.
	msg := []byte{0x19, 0x25, 0x02, 0x01, 0x1B, 0x01, 0x81, 0x01, 'k', 0x01,
		0x17, 1, 2, 3, 4, 5, 6, 7, 8, 0x00}
	got, err := (&thriftReader{data: msg}).readStruct()
	if err != nil {
		t.Fatal(err)
	}
	list := got[1].([]any)
	pairs := got[2].([]thriftPair)
	if len(list) != 2 || list[0] != int64(1) || list[1] != int64(-1) || len(pairs) != 1 ||
		string(pairs[0].Key.([]byte)) != "k" || pairs[0].Value != true || got[3] != nil {
		t.Fatalf("containers %v", got)
	}
	deep := append(bytes.Repeat([]byte{0x1C}, 9), bytes.Repeat([]byte{0}, 10)...)
	if _, err := (&thriftReader{data: deep}).readStruct(); err == nil {
		t.Fatal("accepted a message nested too deeply")
	}
}
