// A loopback ICE-lite media endpoint for the media tests. This is a modified port of
// linux/tests/rtc_fake_edge.py in Meta's Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and
// affiliates, licensed under the Apache License, Version 2.0 (see ../LICENSE). Unlike the SDK's,
// it sends its media once the client nominates a path, so media never goes to a losing path.

package rtc

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
)

type fakeEdge struct {
	t    *testing.T
	port int
	udp  *net.UDPConn
	tcp  net.Listener

	mu        sync.Mutex
	username  string // expected "ServerUfrag:ClientUfrag"; empty until configured
	password  string
	answering bool
	sendMedia bool
	rx, tx    *srtpSession
	checks    []string // transport of each valid check
	sent      bool
	audioOut  [][]byte // Opus packets to send after the nomination
	audioIn   [][]byte // Opus payloads received, in order
	plis      int
	conns     []net.Conn
	changed   chan struct{} // signalled (non-blocking) whenever audioIn or plis change
}

// newFakeEdge listens on 127.0.0.1 over UDP and/or TCP, on one port when both.
func newFakeEdge(t *testing.T, udp, tcp bool) *fakeEdge {
	e := &fakeEdge{t: t, answering: true, sendMedia: true, changed: make(chan struct{}, 1)}
	for attempt := 0; ; attempt++ {
		if e.listen(udp, tcp) {
			break
		}
		if attempt == 20 {
			t.Fatal("could not listen on a shared UDP and TCP port")
		}
	}
	t.Cleanup(e.close)
	return e
}

func (e *fakeEdge) listen(udp, tcp bool) bool {
	if tcp {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			e.t.Fatal(err)
		}
		e.tcp = ln
		e.port = ln.Addr().(*net.TCPAddr).Port
		go e.accept()
	}
	if udp {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: e.port})
		if err != nil {
			e.tcp.Close()
			return false
		}
		e.udp = conn
		e.port = conn.LocalAddr().(*net.UDPAddr).Port
		go e.serveUDP()
	}
	return true
}

func (e *fakeEdge) configure(o *Offer, a *Answer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.username = a.ServerUfrag + ":" + o.ClientUfrag
	e.password = o.ServerPwd
	e.rx, _ = newSRTPSession(o.SRTPKey)
	e.tx, _ = newSRTPSession(o.SRTPKey)
}

func (e *fakeEdge) setAnswering(on bool) {
	e.mu.Lock()
	e.answering = on
	e.mu.Unlock()
}

func (e *fakeEdge) serveUDP() {
	buf := make([]byte, 65536)
	for {
		n, addr, err := e.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := append([]byte(nil), buf[:n]...)
		e.handle(data, func(out []byte) { e.udp.WriteToUDP(out, addr) }, "udp")
	}
}

func (e *fakeEdge) accept() {
	for {
		conn, err := e.tcp.Accept()
		if err != nil {
			return
		}
		e.mu.Lock()
		e.conns = append(e.conns, conn)
		e.mu.Unlock()
		go e.serveTCP(conn)
	}
}

func (e *fakeEdge) serveTCP(conn net.Conn) {
	var wmu sync.Mutex
	send := func(out []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...))
	}
	var size [2]byte
	for {
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		data := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, data); err != nil {
			return
		}
		e.handle(data, send, "tcp")
	}
}

// dropTCP closes every accepted TCP connection.
func (e *fakeEdge) dropTCP() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.conns {
		c.Close()
	}
}

func (e *fakeEdge) handle(data []byte, send func([]byte), kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if isSTUN(data) {
		username, nominated, ok := verifiedRequest(data, e.password)
		if !e.answering || e.username == "" || !ok || username != e.username {
			return
		}
		e.checks = append(e.checks, kind)
		send(stunSuccess(data, e.password))
		if nominated && e.sendMedia && !e.sent {
			e.sent = true
			for _, p := range e.media() {
				send(p)
			}
		}
		return
	}
	if e.rx == nil {
		return
	}
	if isRTCP(data) {
		if plain, err := e.rx.unprotectRTCP(data); err == nil && plain[1] == 206 {
			e.plis++
			e.signal()
		}
		return
	}
	plain, err := e.rx.unprotectRTP(data)
	if err != nil {
		e.t.Errorf("edge: %v", err)
		return
	}
	if p, err := parseRTP(plain); err == nil && p.payloadType == OpusPayloadType {
		e.audioIn = append(e.audioIn, p.payload)
		e.signal()
	}
}

func (e *fakeEdge) signal() {
	select {
	case e.changed <- struct{}{}:
	default:
	}
}

// media is the edge's burst: SPS+PPS in a STAP-A, the IDR in FU-A fragments, then audio.
func (e *fakeEdge) media() [][]byte {
	packets := [][]byte{buildRTP(H264PayloadType, 1, 90000, ServerVideoSSRC, stapA(testSPS, testPPS), false)}
	body := testIDR[1:]
	for i, seq := 0, uint16(2); i < len(body); i, seq = i+900, seq+1 {
		header := byte(5)
		if i == 0 {
			header |= 0x80
		}
		last := i+900 >= len(body)
		if last {
			header |= 0x40
		}
		chunk := body[i:min(i+900, len(body))]
		packets = append(packets, buildRTP(H264PayloadType, seq, 90000, ServerVideoSSRC, fuA(header, chunk), last))
	}
	for i, opus := range e.audioOut {
		packets = append(packets, buildRTP(OpusPayloadType, uint16(100+i), uint32(960*i), ServerAudioSSRC, opus, false))
	}
	var out [][]byte
	for _, p := range packets {
		protected, err := e.tx.protectRTP(p)
		if err != nil {
			e.t.Errorf("edge: %v", err)
			continue
		}
		out = append(out, protected)
	}
	return out
}

func (e *fakeEdge) close() {
	if e.udp != nil {
		e.udp.Close()
	}
	if e.tcp != nil {
		e.tcp.Close()
	}
	e.dropTCP()
}

// verifiedRequest returns a check's USERNAME and whether it nominates, if its integrity verifies.
func verifiedRequest(data []byte, password string) (username string, nominated, ok bool) {
	length := int(binary.BigEndian.Uint16(data[2:]))
	for offset := 20; offset+4 <= 20+length && offset+4 <= len(data); {
		kind := binary.BigEndian.Uint16(data[offset:])
		size := int(binary.BigEndian.Uint16(data[offset+2:]))
		if offset+4+size > len(data) {
			return "", false, false
		}
		switch kind {
		case stunUsername:
			username = string(data[offset+4 : offset+4+size])
		case stunUseCandidate:
			nominated = true
		case stunMessageIntegrity:
			prefix := append([]byte(nil), data[:offset]...)
			binary.BigEndian.PutUint16(prefix[2:], uint16(offset-20+24))
			return username, nominated, string(stunMAC(password, prefix)) == string(data[offset+4:offset+24])
		}
		offset += 4 + size + (4-size%4)%4
	}
	return "", false, false
}
