// Call media over ICE-lite: UDP raced against ICE-TCP (RFC 4571 framing), SRTP, RTP. This is a
// modified port of linux/src/musegadget/rtc/media.py from Meta's Muse Gadget SDK, Copyright (c)
// Meta Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0 (see
// ../LICENSE). Unlike the SDK, it carries Opus packets rather than PCM, and it ends the media
// itself when nothing arrives for a while (the SDK leaves that to video_call.py's watchdog).

package rtc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// RTP payload types and SSRCs of the call's streams.
const (
	OpusPayloadType = 111
	H264PayloadType = 127
	ClientAudioSSRC = 100
	ClientVideoSSRC = 101
	ServerAudioSSRC = 200
	ServerVideoSSRC = 201
	// OpusFrameSamples is one 20 ms frame at 48 kHz: the RTP timestamp step per SendOpus.
	OpusFrameSamples = 960
)

// Reasons the media ends, passed to Events.Closed.
var (
	ErrNoConnectivity = errors.New("rtc: no ICE connectivity to the media edge")
	ErrNoMedia        = errors.New("rtc: no media from the endpoint")
	ErrConsentLost    = errors.New("rtc: connectivity checks went unanswered")
	ErrTransportLost  = errors.New("rtc: the endpoint closed the media connection")
)

// timings are the session's clocks; tests shorten them.
type timings struct {
	connect        time.Duration // Connect's limit when its context has no deadline
	tcpDial        time.Duration
	check          time.Duration // between connectivity checks while racing paths
	consent        time.Duration // between consent checks once connected
	consentTimeout time.Duration // RFC 7675: give up once checks go this long unanswered
	responseWindow time.Duration // an answer older than this is not fresh consent
	noMedia        time.Duration // end the media when nothing arrives for this long
	pliInterval    time.Duration // at most one keyframe request per interval
	tcpWrite       time.Duration // a TCP write blocked this long is abandoned
}

var defaultTimings = timings{
	connect:        10 * time.Second,
	tcpDial:        5 * time.Second,
	check:          200 * time.Millisecond,
	consent:        4 * time.Second,
	consentTimeout: 30 * time.Second,
	responseWindow: 5 * time.Second,
	noMedia:        10 * time.Second,
	pliInterval:    time.Second,
	tcpWrite:       time.Second,
}

// maxPendingChecks bounds the connectivity checks remembered while awaiting answers.
const maxPendingChecks = 64

// Events receives the call's media. Any field may be nil. See the package documentation for
// which goroutine calls them.
type Events struct {
	// Opus gets Muse's audio: one Opus packet (48 kHz mono, normally 20 ms), in order. Late
	// packets are dropped rather than reordered. The slice is the callee's to keep.
	Opus func(packet []byte)
	// Video gets Muse's video: one Annex-B H.264 access unit, starting at a keyframe and
	// skipping any unit damaged by loss. width and height come from the latest SPS, or 0, 0
	// until one has been seen. The slice is the callee's to keep.
	Video func(accessUnit []byte, width, height int)
	// Closed is called once if the media ends by itself after Connect returned: ErrNoMedia,
	// ErrConsentLost or ErrTransportLost. It is not called after Close.
	Closed func(err error)
}

// Stats counts what the session has seen.
type Stats struct {
	Transport   string // "udp" or "tcp"
	BytesIn     int64
	RTPAudio    int
	RTPVideo    int
	RTCP        int
	SRTPErrors  int
	LateAudio   int
	VideoFrames int
	PLIsSent    int
	Started     time.Time
}

// path is one candidate connection to the endpoint.
type path struct {
	tcp  bool
	conn net.Conn
	wmu  sync.Mutex // keeps TCP frames whole
	wait time.Duration
}

func (p *path) kind() string {
	if p.tcp {
		return "tcp"
	}
	return "udp"
}

// send writes one packet; errors are ignored, as loss is (a dead TCP path ends its reader).
func (p *path) send(data []byte) {
	if !p.tcp {
		p.conn.Write(data)
		return
	}
	frame := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(data)), uint16(len(data)))
	frame = append(frame, data...)
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.conn.SetWriteDeadline(time.Now().Add(p.wait))
	p.conn.Write(frame)
}

type pendingCheck struct {
	path *path
	sent time.Time
}

// Media is a connected media session with Muse's media endpoint.
type Media struct {
	offer    *Offer
	ev       Events
	t        timings
	port     int
	username string // "ServerUfrag:ClientUfrag"
	tie      uint64 // ICE tie-breaker

	ctx       context.Context // ends when the session closes; stops dialers and loops
	cancel    context.CancelFunc
	connected chan struct{}
	once      sync.Once

	mu           sync.Mutex // guards everything below up to txMu
	paths        []*path
	pending      map[stunTxID]pendingCheck
	pendingOrder []stunTxID
	selected     *path
	closed       bool
	live         bool // Connect has returned this session
	lastMedia    time.Time
	lastConsent  time.Time
	rx           *srtpSession
	h264         *h264Depacketizer
	audioSeq     uint16
	haveAudioSeq bool
	width        int
	height       int
	stats        Stats

	txMu    sync.Mutex // guards the sending side; taken before mu, never after
	tx      *srtpSession
	seq     uint16
	ts      uint32
	lastPLI time.Time
}

// Connect races ICE-lite connectivity checks over UDP and TCP to the endpoint at host:port (and
// any address the answer names), selects the first path that answers, nominates it and asks for
// a keyframe. ctx bounds only the connection attempt (10 s when it has no deadline); afterwards
// the media runs until Close or until it fails (Events.Closed).
func Connect(ctx context.Context, offer *Offer, answer *Answer, host string, port int, ev Events) (*Media, error) {
	return connect(ctx, offer, answer, host, port, ev, defaultTimings)
}

func connect(ctx context.Context, offer *Offer, answer *Answer, host string, port int, ev Events, t timings) (*Media, error) {
	if offer == nil || answer == nil {
		return nil, errors.New("rtc: Connect needs an offer and an answer")
	}
	m, err := newMedia(offer, answer, port, ev, t)
	if err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.connect)
		defer cancel()
	}
	ips, err := edgeAddrs(ctx, answer, host)
	if err != nil {
		m.Close()
		return nil, err
	}
	for _, ip := range ips {
		go m.openUDP(ip)
		go m.openTCP(ip)
	}
	stopChecks := make(chan struct{})
	go m.checkLoop(stopChecks)
	defer close(stopChecks)
	select {
	case <-m.connected:
	case <-ctx.Done():
		m.Close()
		return nil, fmt.Errorf("%w (%v)", ErrNoConnectivity, ctx.Err())
	}
	selected, err := m.goLive()
	if err != nil {
		return nil, err
	}
	// Racing checks only test paths; nominate the one that won.
	m.check(selected, true)
	go m.monitor()
	m.RequestKeyframe()
	return m, nil
}

func newMedia(offer *Offer, answer *Answer, port int, ev Events, t timings) (*Media, error) {
	rx, err := newSRTPSession(offer.SRTPKey)
	if err != nil {
		return nil, err
	}
	tx, _ := newSRTPSession(offer.SRTPKey)
	var random [14]byte
	rand.Read(random[:])
	ctx, cancel := context.WithCancel(context.Background())
	return &Media{
		offer:     offer,
		ev:        ev,
		t:         t,
		port:      port,
		username:  answer.ServerUfrag + ":" + offer.ClientUfrag,
		tie:       binary.BigEndian.Uint64(random[6:]),
		ctx:       ctx,
		cancel:    cancel,
		connected: make(chan struct{}),
		pending:   map[stunTxID]pendingCheck{},
		rx:        rx,
		h264:      newH264Depacketizer(),
		tx:        tx,
		seq:       binary.BigEndian.Uint16(random[0:]),
		ts:        binary.BigEndian.Uint32(random[2:]),
		stats:     Stats{Started: time.Now()},
	}, nil
}

// edgeAddrs lists the endpoint's addresses: those the answer names first, then the edge host's.
func edgeAddrs(ctx context.Context, answer *Answer, host string) ([]net.IP, error) {
	var ips []net.IP
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if ip != nil && !seen[ip.String()] {
			seen[ip.String()] = true
			ips = append(ips, ip)
		}
	}
	add(net.ParseIP(answer.IPv4))
	add(net.ParseIP(answer.IPv6))
	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	for _, a := range resolved {
		add(a.IP)
	}
	if len(ips) == 0 {
		if err == nil {
			err = errors.New("no addresses")
		}
		return nil, fmt.Errorf("rtc: resolving the media edge: %w", err)
	}
	return ips, nil
}

// goLive keeps only the selected path and starts the media clocks.
func (m *Media) goLive() (*path, error) {
	m.mu.Lock()
	if m.closed || m.selected == nil {
		m.mu.Unlock()
		m.Close()
		return nil, ErrNoConnectivity
	}
	selected := m.selected
	others := m.paths
	m.paths = []*path{selected}
	m.live = true
	m.stats.Transport = selected.kind()
	m.lastMedia, m.lastConsent = time.Now(), time.Now()
	m.mu.Unlock()
	for _, p := range others {
		if p != selected {
			p.conn.Close()
		}
	}
	return selected, nil
}

func (m *Media) openUDP(ip net.IP) {
	network := "udp6"
	if ip.To4() != nil {
		network = "udp4"
	}
	conn, err := net.DialUDP(network, nil, &net.UDPAddr{IP: ip, Port: m.port})
	if err != nil {
		return
	}
	p := &path{conn: conn, wait: m.t.tcpWrite}
	if !m.addPath(p) {
		return
	}
	buf := make([]byte, 65536)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if m.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// A connected UDP socket reports ICMP errors (port unreachable) as read errors;
			// the path just stays unanswered.
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		m.receive(p, buf[:n])
	}
}

func (m *Media) openTCP(ip net.IP) {
	d := net.Dialer{Timeout: m.t.tcpDial}
	conn, err := d.DialContext(m.ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(m.port)))
	if err != nil {
		return
	}
	p := &path{tcp: true, conn: conn, wait: m.t.tcpWrite}
	if !m.addPath(p) {
		return
	}
	var size [2]byte
	for {
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			break
		}
		data := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, data); err != nil {
			break
		}
		m.receive(p, data)
	}
	m.mu.Lock()
	lost := p == m.selected && !m.closed
	m.mu.Unlock()
	if lost {
		m.fail(ErrTransportLost)
	}
}

// addPath adds a newly opened path to the race and checks it at once. It closes the path and
// returns false when the race is already over.
func (m *Media) addPath(p *path) bool {
	m.mu.Lock()
	if m.closed || m.selected != nil {
		m.mu.Unlock()
		p.conn.Close()
		return false
	}
	m.paths = append(m.paths, p)
	m.mu.Unlock()
	m.check(p, false)
	return true
}

// check sends a connectivity check on p and remembers it until answered.
func (m *Media) check(p *path, nominate bool) {
	priority := uint32(90)<<24 | 0xFFFF<<8 | 255
	if !p.tcp {
		priority = uint32(110)<<24 | 0xFFFF<<8 | 255
	}
	packet, txid := bindingRequest(m.username, m.offer.ServerPwd, priority, m.tie, nominate)
	m.mu.Lock()
	m.pending[txid] = pendingCheck{p, time.Now()}
	m.pendingOrder = append(m.pendingOrder, txid)
	for len(m.pendingOrder) > maxPendingChecks {
		delete(m.pending, m.pendingOrder[0])
		m.pendingOrder = m.pendingOrder[1:]
	}
	m.mu.Unlock()
	p.send(packet)
}

// checkLoop re-checks every open path until one is selected.
func (m *Media) checkLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(m.t.check)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		paths := append([]*path(nil), m.paths...)
		m.mu.Unlock()
		for _, p := range paths {
			m.check(p, false)
		}
	}
}

// monitor keeps consent fresh on the selected path and ends the media when consent or media stop.
func (m *Media) monitor() {
	consent := time.NewTicker(m.t.consent)
	defer consent.Stop()
	watch := time.NewTicker(min(100*time.Millisecond, m.t.noMedia/4))
	defer watch.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-consent.C:
			m.mu.Lock()
			selected := m.selected
			stale := time.Since(m.lastConsent) > m.t.consentTimeout
			m.mu.Unlock()
			if selected == nil || stale {
				m.fail(ErrConsentLost)
				return
			}
			m.check(selected, true)
		case <-watch.C:
			m.mu.Lock()
			quiet := time.Since(m.lastMedia) > m.t.noMedia
			m.mu.Unlock()
			if quiet {
				m.fail(ErrNoMedia)
				return
			}
		}
	}
}

// receive handles one packet from path p. It does not keep data.
func (m *Media) receive(p *path, data []byte) {
	now := time.Now()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.stats.BytesIn += int64(len(data))
	if isSTUN(data) {
		m.answered(p, data, now)
		m.mu.Unlock()
		return
	}
	if p != m.selected || len(data) < 12 {
		m.mu.Unlock()
		return
	}
	if isRTCP(data) {
		// The endpoint's reports carry nothing this client acts on.
		m.stats.RTCP++
		m.mu.Unlock()
		return
	}
	// Media comes only from the endpoint's two streams. That also drops our own packets
	// reflected back, which share the call's one SRTP key.
	if ssrc := binary.BigEndian.Uint32(data[8:]); ssrc != ServerAudioSSRC && ssrc != ServerVideoSSRC {
		m.mu.Unlock()
		return
	}
	deliver, wantKeyframe := m.media(data, now)
	m.mu.Unlock()
	if wantKeyframe {
		m.RequestKeyframe()
	}
	if deliver != nil && !m.isClosed() {
		deliver()
	}
}

// answered handles a STUN packet: an authenticated, fresh answer to one of our checks on the same
// path refreshes consent, and the first one selects its path. m.mu is held.
func (m *Media) answered(p *path, data []byte, now time.Time) {
	txid := stunTxID(data[8:20])
	sent, ok := m.pending[txid]
	if !ok || sent.path != p || now.Sub(sent.sent) > m.t.responseWindow ||
		!parseBindingResponse(data, txid, m.offer.ServerPwd) {
		return
	}
	delete(m.pending, txid)
	if p == m.selected {
		m.lastConsent = now
	}
	if m.selected == nil {
		m.selected = p
		m.once.Do(func() { close(m.connected) })
	}
}

// media decrypts one RTP packet from the endpoint and returns what to hand to the caller (to run
// once m.mu is released) and whether to ask for a keyframe. m.mu is held.
func (m *Media) media(data []byte, now time.Time) (deliver func(), wantKeyframe bool) {
	plain, err := m.rx.unprotectRTP(data)
	var packet rtpPacket
	if err == nil {
		packet, err = parseRTP(plain)
	}
	if err != nil {
		m.stats.SRTPErrors++
		return nil, false
	}
	m.lastMedia = now
	switch packet.payloadType {
	case OpusPayloadType:
		m.stats.RTPAudio++
		// Over UDP a packet can arrive after a later one; play in order by dropping it.
		if m.haveAudioSeq && packet.seq-m.audioSeq >= 0x8000 {
			m.stats.LateAudio++
			return nil, false
		}
		m.audioSeq, m.haveAudioSeq = packet.seq, true
		if m.ev.Opus != nil && len(packet.payload) > 0 {
			payload := packet.payload
			return func() { m.ev.Opus(payload) }, false
		}
	case H264PayloadType:
		m.stats.RTPVideo++
		unit := m.h264.push(packet)
		wantKeyframe = m.h264.needsKey
		if unit == nil {
			return nil, wantKeyframe
		}
		m.stats.VideoFrames++
		if w, h, ok := spsDimensions(unit); ok {
			m.width, m.height = w, h
		}
		if m.ev.Video != nil {
			w, h := m.width, m.height
			return func() { m.ev.Video(unit, w, h) }, wantKeyframe
		}
	}
	return nil, wantKeyframe
}

// current returns the selected path, or nil once the media has ended.
func (m *Media) current() *path {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	return m.selected
}

func (m *Media) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// SendOpus sends one Opus packet: one 20 ms frame of 48 kHz mono audio. Call it every 20 ms
// (silence included: Muse ends the caller's turn on a quiet microphone, not on a silent one).
// It does nothing once the media has ended or for an empty packet.
func (m *Media) SendOpus(packet []byte) {
	if len(packet) == 0 {
		return
	}
	m.txMu.Lock()
	defer m.txMu.Unlock()
	selected := m.current()
	if selected == nil {
		return
	}
	rtp := buildRTP(OpusPayloadType, m.seq, m.ts, ClientAudioSSRC, packet, false)
	m.seq++
	m.ts += OpusFrameSamples
	if out, err := m.tx.protectRTP(rtp); err == nil {
		selected.send(out)
	}
}

// RequestKeyframe asks Muse for a keyframe (RTCP PLI), at most once a second. The session asks
// by itself on connect and whenever video is lost; call it when the decoder needs one (for
// example a second time shortly after the first frame, since a player starting up may drop it).
func (m *Media) RequestKeyframe() {
	m.txMu.Lock()
	defer m.txMu.Unlock()
	selected := m.current()
	if selected == nil {
		return
	}
	now := time.Now()
	if now.Sub(m.lastPLI) < m.t.pliInterval {
		return
	}
	m.lastPLI = now
	out, err := m.tx.protectRTCP(buildPLI(ClientVideoSSRC, ServerVideoSSRC))
	if err != nil {
		return
	}
	m.mu.Lock()
	m.stats.PLIsSent++
	m.mu.Unlock()
	selected.send(out)
}

// Transport returns the selected path's transport: "udp" or "tcp".
func (m *Media) Transport() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats.Transport
}

// Stats returns a snapshot of the session's counters.
func (m *Media) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// Close ends the media and releases its sockets. It is idempotent and safe from any goroutine,
// including from inside an Events callback. It does not call Events.Closed.
func (m *Media) Close() { m.shutdown() }

// fail ends the media for err and reports it through Events.Closed, unless it already ended.
func (m *Media) fail(err error) {
	if first, wasLive := m.shutdown(); first && wasLive && m.ev.Closed != nil {
		m.ev.Closed(err)
	}
}

// shutdown closes everything once. It reports whether this call did it, and whether Connect had
// returned the session by then.
func (m *Media) shutdown() (first, wasLive bool) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false, false
	}
	m.closed = true
	wasLive = m.live
	paths := m.paths
	m.paths, m.selected = nil, nil
	m.mu.Unlock()
	m.cancel()
	for _, p := range paths {
		p.conn.Close()
	}
	return true, wasLive
}
