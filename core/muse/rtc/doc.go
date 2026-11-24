// Package rtc is the media layer of a Muse video call: the media-only JOIN that admits the call,
// ICE-lite connectivity (UDP raced against ICE-TCP), SRTP, Opus RTP and H.264 depacketization.
//
// This package is a modified port of linux/src/musegadget/rtc/{join,media,rtp,srtp,stun}.py from
// Meta's Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the
// Apache License, Version 2.0 (see ../LICENSE). The main change from the SDK is that this package
// never touches PCM: Opus encoding and decoding happen outside it (on Android, in MediaCodec), so
// it carries Opus packets in both directions. The SDK's opus.py (a libopus binding) is not ported.
//
// # Flow
//
//  1. Ask the VM for the call's media endpoint (host, port, vip_type) over the signalling channel.
//  2. offer := BuildJoin(threadID, host, port, vipType, video); send offer.Payload (base64) to
//     /voice/calls/join.
//  3. answer := ParseJoinResponse(the returned payload).
//  4. media := Connect(ctx, offer, answer, host, port, events).
//  5. Call media.SendOpus once per 20 ms frame; Muse's audio and video arrive through events.
//  6. media.Close(), then end the call over the signalling channel.
//
// # Concurrency
//
// The Events callbacks run on this package's own goroutines (one reader goroutine delivers all
// media for the selected path, so Opus packets and video units arrive in order and never
// concurrently with each other). They may run before Connect returns, as soon as the path is
// selected. Callbacks must not block for long: they hold up the receive path. They may call any
// Media method, including Close. SendOpus, RequestKeyframe, Transport, Stats and Close are safe to
// call from any goroutine. Once Close has returned, callbacks stop: each checks that the session
// is open just before it runs, so at most one that was already being dispatched may still arrive.
package rtc
