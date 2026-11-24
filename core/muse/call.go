// A realtime voice call with Muse: the caller's audio streams up and Muse's voice streams back on
// one duplex request, and Muse decides when the caller has finished. This follows voice_call.py and
// voice_gate.py in Meta's Muse Gadget SDK (linux/src/musegadget), Copyright (c) Meta Platforms,
// Inc. and affiliates, licensed under the Apache License, Version 2.0. It keeps the wire format and
// the call's lifecycle; playback, echo gating and level metering belong to the device.

package muse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CallRate is the call's audio both ways: 24 kHz, 16-bit little-endian, mono.
const CallRate = 24000

const (
	// callFrameBytes is what one append carries: 960 samples, 40 ms.
	callFrameBytes = 960 * 2
	// maxCallBacklog is how much caller audio may wait for the call to be ready: the wake word's
	// question, said before Muse picked up. Past it the oldest goes.
	maxCallBacklog = 12 * CallRate * 2
	// maxCallAudio bounds one audio delta from Muse, as the SDK does.
	maxCallAudio = CallRate * 2 * 4
	// catchUp is how much faster than real time waiting audio is sent once the call is ready.
	catchUp = 3
)

// ErrCallDeclined means Muse did not let the call in, so the caller may ask the old way.
var ErrCallDeclined = errors.New("muse: Muse did not take the call")

// CallEvents hears a call. Each is called from the call's own goroutine, one at a time, and must
// not block: hand the work to another thread.
type CallEvents struct {
	// Audio is Muse's voice to play, 24 kHz PCM.
	Audio func(pcm []byte)
	// Flush says to drop Muse's voice not yet played: the caller cut in.
	Flush func()
	// Heard is what Muse made of the caller's last turn.
	Heard func(text string)
	// Said is Muse's words so far in the reply being spoken.
	Said func(text string)
	// Hold is Muse asking the device to stop sending audio for now, and then to start again.
	Hold func(held bool)
	// Stop is Muse ending the call, after the caller said goodbye: play out what is queued, then
	// HangUp.
	Stop func()
	// Ended is the call over, with why. It is the last event.
	Ended func(reason string)
}

// CallOptions are a call's limits. Zero means the default.
type CallOptions struct {
	// Idle is how long the call may go with nobody saying anything. 45 seconds.
	Idle time.Duration
	// Overall is the longest a call lasts. 10 minutes.
	Overall time.Duration
}

// Call is one live call. Make it with Client.Call.
type Call struct {
	c       *Client
	vs      *session
	cancel  context.CancelFunc
	ev      CallEvents
	opts    CallOptions
	id      string
	started time.Time
	stream  int64

	frames chan []byte // 40 ms frames for the wire, the backlog included
	ready  chan struct{}

	mu        sync.Mutex
	partial   []byte // caller audio short of a frame
	held      bool
	over      bool
	reason    string
	threadID  string
	lastVoice time.Time // when anybody last spoke, for the idle limit
	playing   string    // the reply whose audio came last (the audio stream's id)
	open      string    // the reply Muse is producing (response.created's id), which a cancel names
	turns     int       // caller turns Muse is still working on
	fenced    map[string]bool
	// kinds counts each kind of record and audioBy the audio of each reply, for the log at the end.
	kinds   map[string]int
	audioBy map[string]int
	said    map[string]*strings.Builder
	endOnce sync.Once
	ended   chan struct{}
}

// callSink splits a call's answer into records and hands them to the call's goroutine.
type callLines struct {
	status   int
	got      chan struct{}
	gotOnce  bool
	buf      []byte
	records  chan map[string]any
	over     bool
	overWhy  string
	overOnce sync.Once
	done     chan struct{}
}

func (l *callLines) onFrame(f frame) {
	if l.over {
		return
	}
	if f.kind == frameResponse {
		l.status = f.status
	}
	if !l.gotOnce && (f.kind == frameResponse || f.kind == frameReset) {
		l.gotOnce = true
		close(l.got)
	}
	if f.kind == frameReset {
		l.finish("reset: " + f.reason)
		return
	}
	l.buf = append(l.buf, f.data...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(l.buf[:i])
		l.buf = l.buf[i+1:]
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		select {
		case l.records <- rec:
		default:
			// The call's goroutine has fallen far behind: better to end the call than to play
			// Muse's voice seconds late.
			l.finish("overrun")
			return
		}
	}
	if len(l.buf) > maxCallAudio*2 {
		l.finish("a record too long")
		return
	}
	if f.endBody {
		l.finish("remote end")
	}
}

func (l *callLines) finish(why string) {
	l.overOnce.Do(func() {
		l.over, l.overWhy = true, why
		close(l.done)
	})
}

// dialVoice opens a voice-only connection to the VM the device's session is on, borrowing that
// session's bearer. It returns once the Noise session is up.
func (c *Client) dialVoice(ctx context.Context) (*session, *voiceTarget, context.CancelFunc, error) {
	if _, err := c.online(); err != nil {
		return nil, nil, nil, err
	}
	c.mu.Lock()
	target := c.voice
	c.mu.Unlock()
	if target == nil {
		return nil, nil, nil, ErrOffline
	}
	url, err := noiseURL(target.host, target.vmID, c.allowPlaintext)
	if err != nil {
		return nil, nil, nil, err
	}
	url += "&app_id=" + voiceAppID
	up := make(chan struct{})
	vs := newSession(sessionParams{
		url:       url,
		bearer:    target.bearer,
		userAgent: c.userAgent,
		tls:       c.cfg.TLS,
		nodeID:    target.nodeID,
		log:       c.log,
		t:         c.t,
		voiceOnly: true,
		up:        func() { close(up) },
	})
	vctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() {
		_, err := vs.run(vctx)
		ran <- err
	}()
	timer := time.NewTimer(35 * time.Second)
	defer timer.Stop()
	select {
	case <-up:
		return vs, target, cancel, nil
	case err := <-ran:
		cancel()
		return nil, nil, nil, fmt.Errorf("muse: the voice connection failed: %v", err)
	case <-ctx.Done():
		cancel()
		return nil, nil, nil, ctx.Err()
	case <-timer.C:
		cancel()
		return nil, nil, nil, errors.New("muse: the voice connection took too long")
	}
}

// Call starts a live call. Audio may be sent at once: what the caller says before Muse is ready
// waits, and is sent faster than real time once it is. It returns when Muse has admitted the call
// and it is ready, or with ErrCallDeclined (wrapped) when Muse would not take it, or another error
// when Muse could not be reached; Ended is not called then.
func (c *Client) Call(ctx context.Context, opts CallOptions, ev CallEvents) (*Call, error) {
	if opts.Idle <= 0 {
		opts.Idle = 45 * time.Second
	}
	if opts.Overall <= 0 {
		opts.Overall = 10 * time.Minute
	}
	k := &Call{
		c: c, ev: ev, opts: opts, id: uuid4(), started: time.Now(),
		frames: make(chan []byte, maxCallBacklog/callFrameBytes),
		ready:  make(chan struct{}),
		fenced: map[string]bool{}, said: map[string]*strings.Builder{},
		kinds: map[string]int{}, audioBy: map[string]int{},
		ended: make(chan struct{}),
	}
	k.lastVoice = time.Now()
	return k, k.start(ctx)
}

// Send takes the caller's audio, 24 kHz PCM, from the microphone.
func (k *Call) Send(pcm []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.over || k.held {
		return
	}
	k.partial = append(k.partial, pcm...)
	for len(k.partial) >= callFrameBytes {
		frame := append([]byte(nil), k.partial[:callFrameBytes]...)
		k.partial = k.partial[callFrameBytes:]
		select {
		case k.frames <- frame:
		default:
			// The backlog is full: the oldest waiting audio goes, never the newest.
			select {
			case <-k.frames:
			default:
			}
			select {
			case k.frames <- frame:
			default:
			}
		}
	}
}

// Interrupt cuts Muse off: the reply being spoken is dropped, and Muse is asked to cancel it.
func (k *Call) Interrupt() {
	k.mu.Lock()
	id, open := k.playing, k.open
	if id != "" {
		k.fenced[id] = true
	}
	k.mu.Unlock()
	if k.ev.Flush != nil {
		k.ev.Flush()
	}
	// Muse cancels by the id it gave the reply when it created it, which is not the audio's.
	if open != "" {
		k.line(map[string]any{"type": "response.cancel", "response_id": open})
	}
}

// HangUp ends the call. It may be called more than once, from any goroutine.
func (k *Call) HangUp() { k.end("hung up") }

// Done is closed once the call is over and cleaned up.
func (k *Call) Done() <-chan struct{} { return k.ended }

func (k *Call) start(ctx context.Context) error {
	vs, target, cancel, err := k.c.dialVoice(ctx)
	if err != nil {
		return err
	}
	k.vs, k.cancel = vs, cancel
	lines := &callLines{got: make(chan struct{}), records: make(chan map[string]any, 512), done: make(chan struct{})}
	var body bytes.Buffer
	for _, rec := range []map[string]any{
		{
			"type": "hatch.voice.call.start", "call_id": k.id, "device": "mcu-wearable",
			"device_id": target.nodeID, "call_media_type": "audio", "proactive_greeting_enabled": false,
		},
		// This device ends the call on a session_control stop, so Muse may end it after a goodbye.
		{"type": "hatch.voice.capability.offer", "end_call_stop": 1},
	} {
		b, _ := json.Marshal(rec)
		body.Write(b)
		body.WriteByte('\n')
	}
	k.stream, err = vs.open(frame{
		kind:    frameRequest,
		verb:    http.MethodPost,
		path:    realtimePath + "?call_id=" + encodeURIComponent(k.id),
		headers: []header{{"Content-Type", "application/x-ndjson; charset=utf-8"}},
		data:    body.Bytes(),
	}, lines)
	if err != nil {
		k.cleanup(false)
		return err
	}

	// Admission: a 2xx, then the readiness record naming this call.
	deadline := time.NewTimer(35 * time.Second)
	defer deadline.Stop()
	select {
	case <-lines.got:
	case <-deadline.C:
		k.cleanup(true)
		return fmt.Errorf("%w: no answer", ErrCallDeclined)
	case <-ctx.Done():
		k.cleanup(true)
		return ctx.Err()
	}
	if lines.status < 200 || lines.status >= 300 {
		k.cleanup(true)
		return fmt.Errorf("%w: HTTP %d", ErrCallDeclined, lines.status)
	}
	for {
		select {
		case rec := <-lines.records:
			switch str(rec, "type") {
			case "hatch.voice.session":
				if str(rec, "call_id") != k.id || str(rec, "session_id") == "" {
					k.cleanup(true)
					return fmt.Errorf("%w: readiness for another call", ErrCallDeclined)
				}
				k.threadID = str(rec, "thread_id")
				close(k.ready)
				k.c.log.Info("muse: call ready", "call", k.id)
				go k.receive(lines)
				go k.send()
				go k.limits()
				return nil
			case "error":
				k.cleanup(true)
				return fmt.Errorf("%w: %s", ErrCallDeclined, errCode(rec))
			}
		case <-lines.done:
			k.cleanup(true)
			return fmt.Errorf("%w: %s before it was ready", ErrCallDeclined, lines.overWhy)
		case <-deadline.C:
			k.cleanup(true)
			return fmt.Errorf("%w: never ready", ErrCallDeclined)
		case <-ctx.Done():
			k.cleanup(true)
			return ctx.Err()
		}
	}
}

// receive follows what Muse says until the call ends.
func (k *Call) receive(lines *callLines) {
	for {
		select {
		case <-k.ended:
			return
		case <-lines.done:
			// Records may still be waiting.
			for {
				select {
				case rec := <-lines.records:
					k.record(rec)
					continue
				default:
				}
				break
			}
			k.end("remote end: " + lines.overWhy)
			return
		case rec := <-lines.records:
			k.record(rec)
		}
	}
}

func (k *Call) record(rec map[string]any) {
	kind := str(rec, "type")
	k.mu.Lock()
	if len(k.kinds) < 64 || k.kinds[kind] > 0 {
		k.kinds[kind]++
	}
	k.mu.Unlock()
	switch kind {
	case "error":
		// Operation errors do not end the media session; a dead session does.
		if errCode(rec) == "session_dead" {
			k.end("session dead")
		} else {
			k.c.log.Warn("muse: the call reported an error", "code", errCode(rec))
		}
		return
	case "hatch.voice.switch_input_hold":
		held, _ := rec["held"].(bool)
		k.mu.Lock()
		k.held = held
		k.partial = nil
		k.mu.Unlock()
		if k.ev.Hold != nil {
			k.ev.Hold(held)
		}
		return
	case "input_audio_buffer.speech_started":
		// The caller is speaking: whatever Muse was saying is cut off (barge-in).
		k.touch()
		k.mu.Lock()
		cut := k.playing != "" && !k.fenced[k.playing]
		k.mu.Unlock()
		if cut {
			k.Interrupt()
		}
		return
	case "conversation.item.input_audio_transcription.completed":
		k.touch()
		if t := strings.TrimSpace(str(rec, "transcript")); t != "" && k.ev.Heard != nil {
			k.ev.Heard(t)
		}
		return
	case "conversation.item.added":
		if item, ok := rec["item"].(map[string]any); ok && str(item, "type") == "session_control" &&
			strings.EqualFold(strings.TrimSpace(str(item, "name")), "stop") {
			k.c.log.Info("muse: Muse ended the call")
			if k.ev.Stop != nil {
				k.ev.Stop()
			} else {
				k.end("goodbye")
			}
		}
		return
	case "response.created":
		k.touch()
		if r, ok := rec["response"].(map[string]any); ok {
			k.mu.Lock()
			k.open = str(r, "id")
			k.mu.Unlock()
		}
		return
	case "response.done", "response.completed":
		k.touch()
		k.mu.Lock()
		if r, ok := rec["response"].(map[string]any); ok && str(r, "id") == k.open {
			k.open = ""
		}
		k.mu.Unlock()
		return
	case "hatch.voice.turn_started":
		k.touch()
		k.mu.Lock()
		k.turns++
		k.mu.Unlock()
		return
	case "hatch.voice.turn_settled":
		k.touch()
		k.mu.Lock()
		if k.turns > 0 {
			k.turns--
		}
		k.mu.Unlock()
		return
	case "response.output_audio_transcript.delta", "response.output_audio_transcript.done":
		id := responseID(rec)
		k.mu.Lock()
		if k.fenced[id] {
			k.mu.Unlock()
			return
		}
		b := k.said[id]
		if b == nil {
			if len(k.said) > 64 {
				k.said = map[string]*strings.Builder{}
			}
			b = &strings.Builder{}
			k.said[id] = b
		}
		if kind == "response.output_audio_transcript.done" {
			if t := str(rec, "transcript"); t != "" {
				b.Reset()
				b.WriteString(t)
			}
		} else {
			b.WriteString(str(rec, "delta"))
		}
		text := b.String()
		k.mu.Unlock()
		if k.ev.Said != nil && strings.TrimSpace(text) != "" {
			k.ev.Said(text)
		}
		return
	}
	pcm, id := audioOf(rec)
	if len(pcm) == 0 {
		return
	}
	k.mu.Lock()
	if id != "" && k.fenced[id] {
		k.mu.Unlock()
		return
	}
	if id != "" {
		k.playing = id
	}
	if len(k.audioBy) < 64 || k.audioBy[id] > 0 {
		k.audioBy[id] += len(pcm)
	}
	k.mu.Unlock()
	if audible(pcm) {
		k.touch()
	}
	if k.ev.Audio != nil {
		k.ev.Audio(pcm)
	}
}

// send puts the caller's frames on the wire: waiting ones faster than real time, then each as it
// comes.
func (k *Call) send() {
	pace := time.Duration(callFrameBytes) * time.Second / time.Duration(CallRate*2) / catchUp
	for {
		select {
		case <-k.ended:
			return
		case frame := <-k.frames:
			k.mu.Lock()
			held := k.held
			k.mu.Unlock()
			if held {
				continue
			}
			if audible(frame) {
				k.touch()
			}
			if !k.line(map[string]any{
				"type":              "input_audio_buffer.append",
				"audio":             base64.StdEncoding.EncodeToString(frame),
				"send_timestamp_ms": time.Now().UnixMilli(),
			}) {
				return
			}
			if len(k.frames) > 0 {
				time.Sleep(pace)
			}
		}
	}
}

// limits ends a call nobody is talking in, and one that has gone on too long.
func (k *Call) limits() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-k.ended:
			return
		case now := <-tick.C:
			k.mu.Lock()
			idle := now.Sub(k.lastVoice)
			if k.turns > 0 {
				// Muse is still working on something the caller asked: that is not idle.
				idle = 0
				k.lastVoice = now
			}
			k.mu.Unlock()
			switch {
			case idle > k.opts.Idle:
				k.end("idle")
				return
			case now.Sub(k.started) > k.opts.Overall:
				k.end("too long")
				return
			}
		}
	}
}

func (k *Call) touch() {
	k.mu.Lock()
	k.lastVoice = time.Now()
	k.mu.Unlock()
}

// line sends one record on the call's body, and says whether it went.
func (k *Call) line(rec map[string]any) bool {
	k.mu.Lock()
	over := k.over
	k.mu.Unlock()
	if over {
		return false
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return false
	}
	if err := k.vs.write(frame{stream: k.stream, kind: frameBody, data: append(b, '\n')}); err != nil {
		k.end("send failed")
		return false
	}
	return true
}

func (k *Call) end(reason string) {
	k.endOnce.Do(func() {
		k.mu.Lock()
		k.over, k.reason = true, reason
		k.mu.Unlock()
		k.c.log.Info("muse: call over", "call", k.id, "reason", reason, "lasted", time.Since(k.started).Round(time.Second))
		k.mu.Lock()
		replies := make([]string, 0, len(k.audioBy))
		for id, n := range k.audioBy {
			// Only a short prefix of each reply's id, and its seconds of audio.
			replies = append(replies, fmt.Sprintf("%s:%.1fs", short(id, 8), float64(n)/float64(CallRate*2)))
		}
		k.c.log.Info("muse: call records", "kinds", fmt.Sprint(k.kinds), "replies", strings.Join(replies, " "))
		k.mu.Unlock()
		go func() {
			k.cleanup(true)
			if k.ev.Ended != nil {
				k.ev.Ended(reason)
			}
		}()
	})
}

// cleanup withdraws the call's stream, tells Muse the call is over if it may have started, and
// closes the voice connection.
func (k *Call) cleanup(admitted bool) {
	defer func() {
		select {
		case <-k.ended:
		default:
			close(k.ended)
		}
	}()
	if k.vs == nil {
		return
	}
	if k.stream != 0 {
		_ = k.vs.write(frame{stream: k.stream, kind: frameReset, code: resetCanceled, reason: "canceled"})
		k.vs.forget(k.stream)
	}
	if admitted {
		end := map[string]any{
			"call_id":         k.id,
			"call_started_at": k.started.UTC().Format("2006-01-02T15:04:05.000+00:00"),
			"call_ended_at":   time.Now().Add(time.Millisecond).UTC().Format("2006-01-02T15:04:05.000+00:00"),
		}
		if k.threadID != "" {
			end["thread_id"] = k.threadID
		}
		b, _ := json.Marshal(end)
		r := &request{done: make(chan struct{})}
		id, err := k.vs.open(frame{
			kind: frameRequest, verb: http.MethodPost, path: endCallPath,
			headers: []header{{"Content-Type", "application/json"}}, data: b, endBody: true,
		}, r)
		if err == nil {
			if k.vs.await(context.Background(), r, 10*time.Second) != nil || r.status/100 != 2 {
				k.c.log.Warn("muse: Muse did not confirm the end of the call", "status", r.status)
			}
			k.vs.forget(id)
		}
	}
	if k.cancel != nil {
		k.cancel()
	}
}

// audioOf is Muse's voice in a record, and which reply it belongs to.
func audioOf(rec map[string]any) ([]byte, string) {
	var encoded string
	switch str(rec, "type") {
	case "continuous_audio_video.delta":
		audio, ok := rec["audio_delta"].(map[string]any)
		if !ok {
			return nil, ""
		}
		if f, ok := audio["format"].(map[string]any); ok {
			if r, ok := f["rate"].(float64); ok && int(r) != CallRate {
				return nil, ""
			}
		}
		encoded = str(audio, "audio_frames")
	case "response.output_audio.delta", "response.audio.delta":
		encoded = str(rec, "delta")
	default:
		return nil, ""
	}
	if encoded == "" || len(encoded) > (maxCallAudio+2)/3*4 {
		return nil, ""
	}
	pcm, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(pcm)%2 != 0 {
		return nil, ""
	}
	return pcm, responseID(rec)
}

func responseID(rec map[string]any) string {
	if id := str(rec, "response_id"); id != "" && len(id) <= 128 {
		return id
	}
	if r, ok := rec["response"].(map[string]any); ok {
		return str(r, "id")
	}
	return ""
}

// audible is the SDK's test for a frame with something in it: RMS of at least 300.
func audible(pcm []byte) bool {
	n := len(pcm) / 2
	if n == 0 {
		return false
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8))
		sum += s * s
	}
	return sum/float64(n) >= 300*300
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func errCode(rec map[string]any) string {
	if e, ok := rec["error"].(map[string]any); ok {
		for _, k := range []string{"code", "type"} {
			if s := str(e, k); s != "" {
				return short(s, 64)
			}
		}
	}
	return short(str(rec, "code"), 64)
}
