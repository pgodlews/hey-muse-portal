// A video call with Muse: Muse's avatar talking, as live H.264, and its voice and the caller's as
// Opus. This follows video_call.py in Meta's Muse Gadget SDK (linux/src/musegadget), Copyright (c)
// Meta Platforms, Inc. and affiliates, licensed under the Apache License, Version 2.0. The call is
// signaled over a voice-only Noise connection (/voice/calls/media-endpoint, /voice/calls/join,
// /voice/end-call) and its media goes straight to Muse's media endpoint as SRTP (package rtc).
//
// Opus is encoded and decoded by the device, not here: the call carries Opus packets.

package muse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/pgodlews/hey-muse-portal/core/muse/rtc"
)

// VideoEvents hears a video call. They are called from the call's own goroutines and must not
// block.
type VideoEvents struct {
	// Opus is one packet of Muse's voice, 48 kHz mono, in order.
	Opus func(packet []byte)
	// Video is one H.264 access unit of Muse's picture, Annex B; width and height are 0 when the
	// media layer does not know them.
	Video func(unit []byte, width, height int)
	// Ended is the call over, with why. It is the last event.
	Ended func(reason string)
}

// VideoCall is one live video call. Make it with Client.VideoCall.
type VideoCall struct {
	c       *Client
	vs      *session
	cancel  context.CancelFunc
	media   *rtc.Media
	id      string
	thread  string
	started time.Time
	ev      VideoEvents
	opts    CallOptions

	endOnce sync.Once
	ended   chan struct{}
	mu      sync.Mutex
	over    bool
	frames  int
}

// VideoCall starts a video call. It returns once the media is connected, or with ErrCallDeclined
// (wrapped) when Muse would not take it, or another error when Muse could not be reached; Ended is
// not called then.
func (c *Client) VideoCall(ctx context.Context, opts CallOptions, ev VideoEvents) (*VideoCall, error) {
	if opts.Overall <= 0 {
		opts.Overall = 10 * time.Minute
	}
	k := &VideoCall{c: c, id: uuid4(), started: time.Now(), ev: ev, opts: opts, ended: make(chan struct{})}
	if err := k.start(ctx); err != nil {
		return nil, err
	}
	go k.limit()
	return k, nil
}

// SendOpus sends one 20 ms packet of the caller's voice, 48 kHz mono.
func (k *VideoCall) SendOpus(packet []byte) {
	k.mu.Lock()
	over := k.over
	k.mu.Unlock()
	if !over && k.media != nil {
		k.media.SendOpus(packet)
	}
}

// RequestKeyframe asks Muse for a whole picture, for a decoder that has lost its place.
func (k *VideoCall) RequestKeyframe() {
	if k.media != nil {
		k.media.RequestKeyframe()
	}
}

// HangUp ends the call. It may be called more than once, from any goroutine.
func (k *VideoCall) HangUp() { k.end("hung up") }

// Done is closed once the call is over and cleaned up.
func (k *VideoCall) Done() <-chan struct{} { return k.ended }

// videoResult unwraps {"ok":true,"result":{...}}, the shape of every answer here.
func videoResult(r *request, what string) (map[string]any, error) {
	if r.status/100 != 2 {
		return nil, fmt.Errorf("%w: %s: HTTP %d", ErrCallDeclined, what, r.status)
	}
	var body struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if json.Unmarshal(r.body, &body) != nil || !body.OK || body.Result == nil {
		return nil, fmt.Errorf("%w: %s failed", ErrCallDeclined, what)
	}
	return body.Result, nil
}

func (k *VideoCall) start(ctx context.Context) error {
	vs, target, cancel, err := k.c.dialVoice(ctx)
	if err != nil {
		return err
	}
	k.vs, k.cancel = vs, cancel
	fail := func(err error, admitted bool) error {
		k.cleanup(admitted)
		return err
	}

	r := &request{done: make(chan struct{})}
	id, err := vs.open(frame{kind: frameRequest, verb: http.MethodGet, path: "/voice/calls/media-endpoint", endBody: true}, r)
	if err != nil {
		return fail(err, false)
	}
	err = vs.await(ctx, r, 20*time.Second)
	vs.forget(id)
	if err != nil {
		return fail(err, false)
	}
	endpoint, err := videoResult(r, "media endpoint")
	if err != nil {
		return fail(err, false)
	}
	host, _ := endpoint["host"].(string)
	portF, _ := endpoint["port"].(float64)
	port := int(portF)
	vip, ok := map[string]int{"META_AI": rtc.EndpointMetaAI, "HATCH": rtc.EndpointHatch}[str(endpoint, "vip_type")]
	if host == "" || port <= 0 || port >= 65536 || float64(port) != portF || !ok {
		return fail(fmt.Errorf("%w: the VM returned an odd media endpoint", ErrCallDeclined), false)
	}

	offer, err := rtc.BuildJoin(k.thread, host, port, vip, true)
	if err != nil {
		return fail(err, false)
	}
	cosmos, _ := endpoint["enable_media_to_cosmos"].(bool)
	join := map[string]any{
		"payload_b64": base64.StdEncoding.EncodeToString(offer.Payload), "call_id": k.id,
		"call_media_type": "video", "device": "mcu-wearable", "device_id": target.nodeID,
		"proactive_greeting_enabled": false, "enable_media_to_cosmos": cosmos,
	}
	body, _ := json.Marshal(join)
	r = &request{done: make(chan struct{})}
	id, err = vs.open(frame{
		kind: frameRequest, verb: http.MethodPost, path: "/voice/calls/join",
		headers: []header{{"Content-Type", "application/json"}}, data: body, endBody: true,
	}, r)
	if err != nil {
		return fail(err, true)
	}
	err = vs.await(ctx, r, 35*time.Second)
	vs.forget(id)
	if err != nil {
		return fail(err, true)
	}
	joined, err := videoResult(r, "call join")
	if err != nil {
		return fail(err, true)
	}
	payload, err := base64.StdEncoding.DecodeString(str(joined, "payload_b64"))
	if err != nil || len(payload) == 0 {
		return fail(fmt.Errorf("%w: Muse admitted no media", ErrCallDeclined), true)
	}
	answer, err := rtc.ParseJoinResponse(payload)
	if err != nil {
		return fail(fmt.Errorf("%w: %v", ErrCallDeclined, err), true)
	}

	connect, stop := context.WithTimeout(ctx, 35*time.Second)
	defer stop()
	media, err := rtc.Connect(connect, offer, answer, host, port, rtc.Events{
		Opus: func(p []byte) {
			if k.ev.Opus != nil {
				k.ev.Opus(p)
			}
		},
		Video: func(unit []byte, w, h int) {
			k.mu.Lock()
			k.frames++
			first := k.frames == 1
			k.mu.Unlock()
			if first {
				// The first picture may not be whole; ask for one that is, a moment later.
				time.AfterFunc(time.Second, k.RequestKeyframe)
			}
			if k.ev.Video != nil {
				k.ev.Video(unit, w, h)
			}
		},
		Closed: func(err error) { k.end(fmt.Sprint("media ended: ", err)) },
	})
	if err != nil {
		return fail(fmt.Errorf("muse: the call's media did not connect: %w", err), true)
	}
	k.media = media
	k.c.log.Info("muse: video call connected", "call", k.id, "transport", media.Transport())
	return nil
}

func (k *VideoCall) limit() {
	timer := time.NewTimer(k.opts.Overall)
	defer timer.Stop()
	select {
	case <-k.ended:
	case <-timer.C:
		k.end("too long")
	}
}

func (k *VideoCall) end(reason string) {
	k.endOnce.Do(func() {
		k.mu.Lock()
		k.over = true
		frames := k.frames
		k.mu.Unlock()
		k.c.log.Info("muse: video call over", "call", k.id, "reason", reason, "frames", frames,
			"lasted", time.Since(k.started).Round(time.Second))
		go func() {
			k.cleanup(true)
			if k.ev.Ended != nil {
				k.ev.Ended(reason)
			}
		}()
	})
}

// cleanup stops the media, tells Muse the call is over if it may have been admitted, and closes the
// voice connection.
func (k *VideoCall) cleanup(admitted bool) {
	defer func() {
		select {
		case <-k.ended:
		default:
			close(k.ended)
		}
	}()
	if k.media != nil {
		k.media.Close()
	}
	if k.vs != nil && admitted {
		end := map[string]any{
			"call_id":         k.id,
			"call_started_at": k.started.UTC().Format("2006-01-02T15:04:05.000+00:00"),
			"call_ended_at":   time.Now().Add(time.Millisecond).UTC().Format("2006-01-02T15:04:05.000+00:00"),
		}
		if k.thread != "" {
			end["thread_id"] = k.thread
		}
		b, _ := json.Marshal(end)
		r := &request{done: make(chan struct{})}
		id, err := k.vs.open(frame{
			kind: frameRequest, verb: http.MethodPost, path: endCallPath,
			headers: []header{{"Content-Type", "application/json"}}, data: b, endBody: true,
		}, r)
		if err == nil {
			if k.vs.await(context.Background(), r, 10*time.Second) != nil || r.status/100 != 2 ||
				!bytes.Contains(r.body, []byte(`"ok":true`)) {
				k.c.log.Warn("muse: Muse did not confirm the end of the video call", "status", r.status)
			}
			k.vs.forget(id)
		}
	}
	if k.cancel != nil {
		k.cancel()
	}
}
