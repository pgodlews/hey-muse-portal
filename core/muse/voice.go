// Muse's own voice: the TTS stream for a voice-modality reply, and a voice-only connection that
// asks whether Muse admits a realtime call. Both follow linux/src/musegadget/link_client.py and
// voice_call.py in Meta's Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates,
// licensed under the Apache License, Version 2.0.

package muse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	ttsPath       = "/api/voice/tts-stream"
	ttsLegacyPath = "/voice/tts-stream"
	realtimePath  = "/voice/realtime"
	endCallPath   = "/voice/end-call"
	// voiceAppID is the app id a voice-only connection names, as the SDK's calls do.
	voiceAppID = "hatch-web"
)

// voiceTarget is where a voice-only connection goes: the VM the device's session is on, with the
// bearer that session used.
type voiceTarget struct{ host, vmID, bearer, nodeID string }

// replyIDs are the ids of the messages that make up the turn's answer, in order.
func (s *session) replyIDs(t *turn) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(t.messages))
	for _, m := range t.messages {
		if m.id != "" {
			ids = append(ids, m.id)
		}
	}
	return ids
}

// serveVoiceOnly carries the streams of a voice-only connection until it drops. It never opens the
// control stream or registers, so the device's own session stays the device's.
func (s *session) serveVoiceOnly() (outcome, error) {
	rx := func() { _ = s.ws.SetReadDeadline(time.Now().Add(s.p.t.rxTimeout)) }
	s.ws.SetPongHandler(func(string) error { rx(); return nil })
	rx()
	s.wg.Go(s.keepalive)
	if s.p.up != nil {
		s.p.up()
	}
	for {
		kind, data, err := s.ws.ReadMessage()
		if err != nil {
			return outcomeClosed, fmt.Errorf("muse: connection closed: %w", err)
		}
		rx()
		if kind != websocket.BinaryMessage {
			continue
		}
		plain, err := s.recvCipher.Decrypt(nil, nil, data)
		if err != nil {
			return outcomeClosed, errors.New("muse: a message from the VM did not decrypt")
		}
		envelope, err := s.chunks.add(plain)
		if err != nil {
			return outcomeClosed, err
		}
		if envelope == nil {
			continue
		}
		f, err := decodeServiceResponse(envelope)
		if err != nil {
			return outcomeClosed, err
		}
		s.mu.Lock()
		if to := s.streams[f.stream]; to != nil {
			to.onFrame(f)
		}
		s.mu.Unlock()
	}
}

// SpeakReply fetches Muse's own speech for a reply Muse gave in voice (ask with SpokenText), as MP3.
// It returns the path that answered, the HTTP status and the audio. The SDK's newer path is tried
// first, and the older one when that one is not there.
func (c *Client) SpeakReply(ctx context.Context, messageID string) (path string, status int, mp3 []byte, err error) {
	s, err := c.online()
	if err != nil {
		return "", 0, nil, err
	}
	for _, p := range []string{ttsPath, ttsLegacyPath} {
		r := &request{done: make(chan struct{})}
		id, err := s.open(frame{
			kind: frameRequest,
			verb: http.MethodGet,
			path: p + "?message_id=" + encodeURIComponent(messageID),
			headers: []header{
				{"Accept", "audio/mpeg"},
				{"x-request-id", uuid4()},
				{"x-app-id", appID},
			},
			endBody: true,
		}, r)
		if err != nil {
			return p, 0, nil, err
		}
		err = s.await(ctx, r, time.Minute)
		s.forget(id)
		if err != nil {
			return p, r.status, nil, err
		}
		if r.status == http.StatusNotFound && p == ttsPath {
			continue
		}
		return p, r.status, r.body, nil
	}
	return ttsLegacyPath, http.StatusNotFound, nil, nil
}

// CallProbe is what Muse said to a realtime call that was started and at once ended.
type CallProbe struct {
	// Connected is whether the voice-only connection came up; ConnectErr says why not.
	Connected  bool   `json:"connected"`
	ConnectErr string `json:"connect_err,omitempty"`
	// Status answers POST /voice/realtime: 2xx admits the call. Error is its error code, if any.
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
	// Events are the kinds of the call's first records, in the order they first came.
	Events []string `json:"events,omitempty"`
	// AudioBytes counts the audio in those records, base64 decoded size estimated.
	AudioBytes int `json:"audio_bytes,omitempty"`
	// EndCall answers POST /voice/end-call.
	EndCall     int    `json:"end_call"`
	EndCallBody string `json:"end_call_body,omitempty"`
}

// callSink keeps the start of a call's answer: its status and its first records.
type callSink struct {
	status  int
	body    []byte
	got     chan struct{} // closed on the response frame
	over    bool
	gotOnce bool
}

func (c *callSink) onFrame(f frame) {
	if c.over {
		return
	}
	if f.kind == frameReset {
		c.over = true
		if !c.gotOnce {
			c.gotOnce = true
			close(c.got)
		}
		return
	}
	if f.kind == frameResponse {
		c.status = f.status
		if !c.gotOnce {
			c.gotOnce = true
			close(c.got)
		}
	}
	if len(c.body) < 1<<20 {
		c.body = append(c.body, f.data...)
	}
	if f.endBody {
		c.over = true
	}
}

// ProbeCall asks Muse to start a realtime voice call on a voice-only connection, sends no audio,
// waits a few seconds for what Muse says, and ends the call. The device's session is not touched.
// An admitted call may replace a call already on the VM.
func (c *Client) ProbeCall(ctx context.Context) CallProbe {
	var out CallProbe
	vs, target, cancel, err := c.dialVoice(ctx)
	if err != nil {
		out.ConnectErr = err.Error()
		return out
	}
	defer cancel()
	out.Connected = true

	callID := uuid4()
	started := time.Now()
	lines := ""
	for _, line := range []map[string]any{
		{
			"type": "hatch.voice.call.start", "call_id": callID, "device": "mcu-wearable",
			"device_id": target.nodeID, "call_media_type": "audio", "proactive_greeting_enabled": false,
		},
		{"type": "hatch.voice.capability.offer", "end_call_stop": 1},
	} {
		b, _ := json.Marshal(line)
		lines += string(b) + "\n"
	}
	sink := &callSink{got: make(chan struct{})}
	id, err := vs.open(frame{
		kind:    frameRequest,
		verb:    http.MethodPost,
		path:    realtimePath + "?call_id=" + encodeURIComponent(callID),
		headers: []header{{"Content-Type", "application/x-ndjson; charset=utf-8"}},
		data:    []byte(lines),
	}, sink)
	if err != nil {
		out.ConnectErr = err.Error()
		return out
	}
	select {
	case <-sink.got:
	case <-time.After(15 * time.Second):
	}
	// What a call says first, if it was let in.
	time.Sleep(5 * time.Second)
	_ = vs.write(frame{stream: id, kind: frameReset, code: resetCanceled, reason: "canceled"})
	vs.mu.Lock()
	out.Status, out.Events, out.AudioBytes, out.Error = sink.status, nil, 0, ""
	seen := map[string]bool{}
	for _, line := range strings.Split(string(sink.body), "\n") {
		var rec struct {
			Type  string          `json:"type"`
			Audio string          `json:"audio"`
			Delta string          `json:"delta"`
			Error json.RawMessage `json:"error"`
			Code  string          `json:"code"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Type != "" && !seen[rec.Type] && len(out.Events) < 24 {
			seen[rec.Type] = true
			out.Events = append(out.Events, rec.Type)
		}
		out.AudioBytes += (len(rec.Audio) + len(rec.Delta)) * 3 / 4
		if out.Error == "" {
			out.Error = errorCode(rec.Error, rec.Code)
		}
	}
	vs.mu.Unlock()
	vs.forget(id)

	end, _ := json.Marshal(map[string]any{
		"call_id":         callID,
		"call_started_at": started.UTC().Format("2006-01-02T15:04:05.000+00:00"),
		"call_ended_at":   time.Now().UTC().Format("2006-01-02T15:04:05.000+00:00"),
	})
	r := &request{done: make(chan struct{})}
	eid, err := vs.open(frame{
		kind:    frameRequest,
		verb:    http.MethodPost,
		path:    endCallPath,
		headers: []header{{"Content-Type", "application/json"}},
		data:    end,
		endBody: true,
	}, r)
	if err == nil && vs.await(ctx, r, 10*time.Second) == nil {
		out.EndCall = r.status
		out.EndCallBody = short(string(r.body), 200)
	}
	vs.forget(eid)
	return out
}

// errorCode is a short error code out of an error record, if it has one.
func errorCode(raw json.RawMessage, code string) string {
	if code != "" {
		return short(code, 64)
	}
	var obj struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		for _, v := range []string{obj.Code, obj.Type, obj.Message} {
			if v != "" {
				return short(v, 120)
			}
		}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return short(s, 120)
	}
	return ""
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// online is the device's registered session.
func (c *Client) online() (*session, error) {
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s == nil {
		return nil, ErrOffline
	}
	s.mu.Lock()
	registered := !s.registeredAt.IsZero()
	s.mu.Unlock()
	if !registered {
		return nil, ErrOffline
	}
	return s, nil
}

// await waits for one request's answer.
func (s *session) await(ctx context.Context, r *request, limit time.Duration) error {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ended:
		return ErrOffline
	case <-timer.C:
		return errors.New("muse: no answer in time")
	}
}
