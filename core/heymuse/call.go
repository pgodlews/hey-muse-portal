package heymuse

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pgodlews/hey-muse-portal/core/muse"
)

// CallRate is the rate of Muse's voice in a call, which OnCallAudio delivers: 24 kHz, 16-bit
// little-endian, mono.
const CallRate = muse.CallRate

// How a call ended, in OnCallEnded. Anything else is a reason in words, for the log.
const (
	// CallDeclined: Muse would not take the call. Ask the old way.
	CallDeclined = "declined"
	// CallUnreachable: Muse could not be reached for a call.
	CallUnreachable = "unreachable"
)

// activeCall is the live call, if there is one.
type activeCall struct {
	id     int
	cancel context.CancelFunc
	k      *muse.Call
	v      *muse.VideoCall
	up     upsampler
	// pending holds what the microphone sent before the call existed.
	pending [][]byte
}

// StartCall starts a live call with Muse and returns its number. The microphone's audio goes to
// CallAudio from now on; what is said before Muse picks up waits and is not lost. The Host then
// hears OnCallReady, Muse's voice in OnCallAudio, and finally OnCallEnded. A call Muse would not
// take ends with CallDeclined before it was ever ready, and the question can then be asked the old
// way.
func (a *App) StartCall() int {
	a.CancelTurn()
	a.HangUp()
	a.mu.Lock()
	a.turnID++
	id := a.turnID
	var c *muse.Client
	if a.client != nil && !a.client.stopping {
		c = a.client.c
	}
	ctx, cancel := context.WithCancel(context.Background())
	if a.ctx != nil {
		ctx, cancel = context.WithCancel(a.ctx)
	}
	call := &activeCall{id: id, cancel: cancel}
	a.call = call
	a.mu.Unlock()

	if c == nil {
		a.mu.Lock()
		a.call = nil
		a.mu.Unlock()
		cancel()
		a.host.OnCallEnded(id, CallUnreachable, false)
		return id
	}
	go func() {
		k, err := c.Call(ctx, muse.CallOptions{}, muse.CallEvents{
			Audio: func(pcm []byte) { a.host.OnCallAudio(id, pcm) },
			Flush: func() { a.host.OnCallFlush(id) },
			Heard: func(text string) { a.host.OnCallHeard(id, strings.TrimSpace(text)) },
			Said:  func(text string) { a.host.OnCallSaid(id, strings.TrimSpace(text)) },
			Hold:  func(held bool) { a.host.OnCallHold(id, held) },
			Stop:  func() { a.host.OnCallStop(id) },
			Ended: func(reason string) {
				a.mu.Lock()
				if a.call == call {
					a.call = nil
				}
				a.mu.Unlock()
				cancel()
				a.host.OnCallEnded(id, reason, true)
			},
		})
		if err != nil {
			a.mu.Lock()
			if a.call == call {
				a.call = nil
			}
			a.mu.Unlock()
			cancel()
			reason := CallUnreachable
			if errors.Is(err, muse.ErrCallDeclined) {
				reason = CallDeclined
			}
			slog.Warn("muse: no call", "err", err)
			a.host.OnCallEnded(id, reason, false)
			return
		}
		a.mu.Lock()
		live := a.call == call
		if live {
			call.k = k
			for _, p := range call.pending {
				k.Send(p)
			}
			call.pending = nil
		}
		a.mu.Unlock()
		if !live {
			// Hung up while it was being set up.
			k.HangUp()
			return
		}
		a.host.OnCallReady(id)
	}()
	return id
}

// CallAudio takes the microphone's audio, 16 kHz 16-bit little-endian mono, for the live call.
// Leave out what should not reach Muse, such as the Portal's own voice when the room hears it.
func (a *App) CallAudio(pcm []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	call := a.call
	if call == nil {
		return
	}
	up := call.up.feed(samplesOf(pcm))
	if call.k == nil {
		// Muse has not picked up yet: keep at most 12 seconds.
		call.pending = append(call.pending, up)
		for total(call.pending) > 12*CallRate*2 {
			call.pending = call.pending[1:]
		}
		return
	}
	call.k.Send(up)
}

// InterruptCall cuts Muse off in the middle of saying something.
func (a *App) InterruptCall() {
	a.mu.Lock()
	call := a.call
	a.mu.Unlock()
	if call != nil && call.k != nil {
		call.k.Interrupt()
	}
}

// HangUp ends the live call, if there is one. OnCallEnded follows.
func (a *App) HangUp() {
	a.mu.Lock()
	call := a.call
	a.mu.Unlock()
	if call == nil {
		return
	}
	switch {
	case call.k != nil:
		call.k.HangUp()
	case call.v != nil:
		call.v.HangUp()
	default:
		call.cancel()
	}
}

// InCall says whether a call is live or being set up.
func (a *App) InCall() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.call != nil
}

func total(chunks [][]byte) int {
	n := 0
	for _, c := range chunks {
		n += len(c)
	}
	return n
}

// upsampler brings 16 kHz up to the call's 24 kHz across chunk boundaries: three samples out for
// every two in, by linear interpolation.
type upsampler struct {
	held  []int16
	phase int
}

func (u *upsampler) feed(in []int16) []byte {
	s := append(u.held, in...)
	out := make([]byte, 0, len(in)*3+6)
	put := func(v float64) {
		x := int16(max(-32768, min(32767, v)))
		out = append(out, byte(x), byte(uint16(x)>>8))
	}
	// Output sample j sits at input position j*2/3; walk it in steps of 2/3.
	i := 0
	for ; i+1 < len(s); i += 2 {
		a, b := float64(s[i]), float64(s[i+1])
		var c float64
		if i+2 < len(s) {
			c = float64(s[i+2])
		} else {
			break
		}
		put(a)
		put(a + (b-a)*2/3)
		put(b + (c-b)*1/3)
	}
	u.held = append(u.held[:0], s[i:]...)
	return out
}

// StartVideoCall starts a video call with Muse: Muse's avatar talks on screen (OnCallVideo) and its
// voice comes as Opus (OnCallOpus); the caller's voice goes to CallOpus, encoded by the device. It
// ends like StartCall's: OnCallReady once connected, OnCallEnded at the end, and CallDeclined when
// Muse would not take it, so a voice call can be tried instead.
func (a *App) StartVideoCall() int {
	a.CancelTurn()
	a.HangUp()
	a.mu.Lock()
	a.turnID++
	id := a.turnID
	var c *muse.Client
	if a.client != nil && !a.client.stopping {
		c = a.client.c
	}
	ctx, cancel := context.WithCancel(context.Background())
	if a.ctx != nil {
		ctx, cancel = context.WithCancel(a.ctx)
	}
	call := &activeCall{id: id, cancel: cancel}
	a.call = call
	a.mu.Unlock()

	if c == nil {
		a.mu.Lock()
		a.call = nil
		a.mu.Unlock()
		cancel()
		a.host.OnCallEnded(id, CallUnreachable, false)
		return id
	}
	go func() {
		v, err := c.VideoCall(ctx, muse.CallOptions{}, muse.VideoEvents{
			Opus:  func(p []byte) { a.host.OnCallOpus(id, p) },
			Video: func(u []byte, w, h int) { a.host.OnCallVideo(id, u, w, h) },
			Ended: func(reason string) {
				a.mu.Lock()
				if a.call == call {
					a.call = nil
				}
				a.mu.Unlock()
				cancel()
				a.host.OnCallEnded(id, reason, true)
			},
		})
		if err != nil {
			a.mu.Lock()
			if a.call == call {
				a.call = nil
			}
			a.mu.Unlock()
			cancel()
			reason := CallUnreachable
			if errors.Is(err, muse.ErrCallDeclined) {
				reason = CallDeclined
			}
			slog.Warn("muse: no video call", "err", err)
			a.host.OnCallEnded(id, reason, false)
			return
		}
		a.mu.Lock()
		live := a.call == call
		if live {
			call.v = v
		}
		a.mu.Unlock()
		if !live {
			v.HangUp()
			return
		}
		a.host.OnCallReady(id)
	}()
	return id
}

// CallOpus sends one 20 ms Opus packet of the caller's voice, 48 kHz mono, in a video call.
func (a *App) CallOpus(packet []byte) {
	a.mu.Lock()
	call := a.call
	a.mu.Unlock()
	if call != nil && call.v != nil {
		call.v.SendOpus(packet)
	}
}

// CallKeyframe asks Muse for a whole picture in a video call, for a decoder that lost its place.
func (a *App) CallKeyframe() {
	a.mu.Lock()
	call := a.call
	a.mu.Unlock()
	if call != nil && call.v != nil {
		call.v.RequestKeyframe()
	}
}
