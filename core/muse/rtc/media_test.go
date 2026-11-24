// Media tests against a loopback ICE-lite endpoint, ported from linux/tests/test_video_call.py in
// Meta's Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the
// Apache License, Version 2.0 (see ../LICENSE), with changes: Opus travels as packets here.

package rtc

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type videoUnit struct {
	unit          []byte
	width, height int
}

// recorder collects what a session hands out.
type recorder struct {
	mu     sync.Mutex
	opus   [][]byte
	video  []videoUnit
	closed chan error
}

func newRecorder() *recorder { return &recorder{closed: make(chan error, 4)} }

func (r *recorder) events() Events {
	return Events{
		Opus: func(p []byte) {
			r.mu.Lock()
			r.opus = append(r.opus, p)
			r.mu.Unlock()
		},
		Video: func(unit []byte, w, h int) {
			r.mu.Lock()
			r.video = append(r.video, videoUnit{unit, w, h})
			r.mu.Unlock()
		},
		Closed: func(err error) { r.closed <- err },
	}
}

func (r *recorder) counts() (opus, video int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opus), len(r.video)
}

func (r *recorder) expectClosed(t *testing.T, want error) {
	t.Helper()
	select {
	case err := <-r.closed:
		if !errors.Is(err, want) {
			t.Fatalf("closed with %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("media did not end with %v", want)
	}
	select {
	case err := <-r.closed:
		t.Fatalf("Closed called twice (%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// edgeCall builds a JOIN for edge, answers it the way the endpoint would and returns the pair.
func edgeCall(t *testing.T, edge *fakeEdge) (*Offer, *Answer) {
	t.Helper()
	offer, err := BuildJoin(testThread, "127.0.0.1", edge.port, EndpointMetaAI, true)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := ParseJoinResponse(joinResponse(readOffer(t, offer.Payload), "updatedServerUfrag1"))
	if err != nil {
		t.Fatal(err)
	}
	edge.configure(offer, answer)
	return offer, answer
}

func dial(t *testing.T, edge *fakeEdge, rec *recorder, tm timings) *Media {
	t.Helper()
	offer, answer := edgeCall(t, edge)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := connect(ctx, offer, answer, "127.0.0.1", edge.port, rec.events(), tm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestMediaConnectsAndCarriesAudioAndVideo(t *testing.T) {
	for _, mode := range []struct {
		name     string
		udp, tcp bool
	}{{"udp+tcp", true, true}, {"tcp", false, true}, {"udp", true, false}} {
		t.Run(mode.name, func(t *testing.T) {
			edge := newFakeEdge(t, mode.udp, mode.tcp)
			for i := range 5 {
				edge.audioOut = append(edge.audioOut, []byte{0xF8, byte(i), 1, 2, 3})
			}
			rec := newRecorder()
			m := dial(t, edge, rec, defaultTimings)

			switch transport := m.Transport(); {
			case !mode.udp && transport != "tcp", !mode.tcp && transport != "udp":
				t.Fatalf("transport %q", transport)
			case transport != "udp" && transport != "tcp":
				t.Fatalf("transport %q", transport)
			}

			eventually(t, "Muse's audio and video", func() bool {
				opus, video := rec.counts()
				return opus == 5 && video == 1
			})
			rec.mu.Lock()
			if v := rec.video[0]; !bytes.Equal(v.unit, wantUnit) || v.width != 448 || v.height != 768 {
				t.Fatalf("video unit %dx%d", v.width, v.height)
			}
			for i, p := range rec.opus {
				if !bytes.Equal(p, edge.audioOut[i]) {
					t.Fatalf("Opus packet %d: % x", i, p)
				}
			}
			rec.mu.Unlock()

			edge.mu.Lock()
			plis := edge.plis
			edge.mu.Unlock()
			if plis < 1 {
				t.Fatal("no keyframe request on connect")
			}

			var sent [][]byte
			for i := range 10 {
				p := []byte{0xF8, byte(i), 0xAA}
				sent = append(sent, p)
				m.SendOpus(p)
			}
			eventually(t, "our audio at the edge", func() bool {
				edge.mu.Lock()
				defer edge.mu.Unlock()
				return len(edge.audioIn) == len(sent)
			})
			edge.mu.Lock()
			for i, p := range edge.audioIn {
				if !bytes.Equal(p, sent[i]) {
					t.Fatalf("edge got packet %d: % x", i, p)
				}
			}
			edge.mu.Unlock()

			if s := m.Stats(); s.VideoFrames != 1 || s.RTPAudio != 5 || s.SRTPErrors != 0 || s.PLIsSent < 1 {
				t.Fatalf("stats %+v", s)
			}
			m.Close()
			m.Close()
			m.SendOpus([]byte{1}) // ignored once closed
			m.RequestKeyframe()
			select {
			case err := <-rec.closed:
				t.Fatalf("Closed called after Close (%v)", err)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestMediaFailsCleanlyWithoutAReachableEndpoint(t *testing.T) {
	edge := newFakeEdge(t, true, false) // never configured: every check goes unanswered
	offer, err := BuildJoin(testThread, "127.0.0.1", edge.port, EndpointMetaAI, true)
	if err != nil {
		t.Fatal(err)
	}
	answer := &Answer{ServerUfrag: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	m, err := Connect(ctx, offer, answer, "127.0.0.1", edge.port, Events{})
	if !errors.Is(err, ErrNoConnectivity) || m != nil {
		t.Fatalf("connect: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("connect overran its context")
	}
}

func TestMediaIgnoresReportsReflectionsAndLateAudio(t *testing.T) {
	edge := newFakeEdge(t, true, true)
	rec := newRecorder()
	m := dial(t, edge, rec, defaultTimings)
	eventually(t, "video", func() bool { _, video := rec.counts(); return video == 1 })

	peer := mustSession(t, m.offer.SRTPKey) // the endpoint's side, fresh replay state
	selected := m.current()
	for _, c := range []struct {
		seq  uint16
		ssrc uint32
	}{{10, ServerAudioSSRC}, {9, ServerAudioSSRC}, {11, ClientAudioSSRC}} { // in order, late, reflected
		p, err := peer.protectRTP(buildRTP(OpusPayloadType, c.seq, 960*uint32(c.seq), c.ssrc, []byte{0xF8}, false))
		if err != nil {
			t.Fatal(err)
		}
		m.receive(selected, p)
	}
	report, _ := peer.protectRTCP([]byte{0x80, 201, 0, 1, 0, 0, 0, 1})
	m.receive(selected, report)

	if opus, _ := rec.counts(); opus != 1 {
		t.Fatalf("%d Opus packets handed out, want 1", opus)
	}
	if s := m.Stats(); s.LateAudio != 1 || s.RTCP != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestMediaEndsWhenConsentChecksGoUnanswered(t *testing.T) {
	tm := defaultTimings
	tm.consent, tm.consentTimeout = 50*time.Millisecond, 200*time.Millisecond
	edge := newFakeEdge(t, false, true)
	rec := newRecorder()
	m := dial(t, edge, rec, tm)
	edge.setAnswering(false) // the endpoint stops answering
	rec.expectClosed(t, ErrConsentLost)
	m.SendOpus([]byte{1}) // harmless after the media ended
	m.Close()
}

func TestMediaEndsWhenNoMediaArrives(t *testing.T) {
	tm := defaultTimings
	tm.noMedia = 300 * time.Millisecond
	edge := newFakeEdge(t, true, false)
	edge.sendMedia = false
	rec := newRecorder()
	dial(t, edge, rec, tm)
	rec.expectClosed(t, ErrNoMedia)
}

func TestMediaEndsWhenTheTCPConnectionDrops(t *testing.T) {
	edge := newFakeEdge(t, false, true)
	rec := newRecorder()
	m := dial(t, edge, rec, defaultTimings)
	if m.Transport() != "tcp" {
		t.Fatalf("transport %q", m.Transport())
	}
	edge.dropTCP()
	rec.expectClosed(t, ErrTransportLost)
}

func TestStaleCheckResponsesAreNotConsent(t *testing.T) {
	edge := newFakeEdge(t, false, true)
	rec := newRecorder()
	m := dial(t, edge, rec, defaultTimings)
	m.mu.Lock()
	before := m.lastConsent
	m.t.responseWindow = 0
	m.mu.Unlock()
	m.check(m.current(), false)
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	after := m.lastConsent
	m.mu.Unlock()
	if !after.Equal(before) {
		t.Fatal("a stale answer counted as consent")
	}
}

func TestCloseIsSafeFromManyGoroutines(t *testing.T) {
	edge := newFakeEdge(t, true, true)
	rec := newRecorder()
	m := dial(t, edge, rec, defaultTimings)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				m.SendOpus([]byte{0xF8, byte(i)})
				m.RequestKeyframe()
				_ = m.Stats()
			}
			m.Close()
		}()
	}
	wg.Wait()
	if !m.isClosed() {
		t.Fatal("not closed")
	}
}
