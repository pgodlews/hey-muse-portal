# Backlog

Ideas from Meta's Muse Gadget SDK (github.com/facebookincubator/muse-gadget-sdk), surveyed on
2026-11-24 against this app. "SDK:" points at where the SDK does it. Effort: S small, M medium,
L large.

## Wanted next

- [ ] **Muse shows pictures on the Portal** (M). Device commands `display.draw_url`,
  `display.draw_file` (a file on the VM, read with `/fs/raw` as the avatar is) and
  `display.show_animation` (back to the avatar). The picture covers the avatar until a tap or
  `show_animation`. Works during a live call too: commands arrive on the main session.
  SDK: `esp32/main/noise_control.cpp` (display.* registrations), `esp32/devices/guides/muse.md`.
- [ ] **Camera** (M). `camera.capture`: one JPEG, base64 in the command's result, only when Muse
  asks; works during a live call. Respect the Portal's privacy shutter and show that a photo was
  taken. Also **photo with a voice note** for the one-question mode: a photo taken as the note
  starts, sent as `{"type":"image",...}` after the audio in the same `/chat/stream` body (256 KB
  max), with a "taking a photo / photo taken / no photo" caption. Off by default.
  SDK: `noise_control.cpp` (camera.capture), `components/muse/muse_photo_note.c`.

## Live calls: fixes

- [x] Barge-in cancels by `response.created`'s id, not the audio's. (done on feat/live-calls)
- [x] Idle timer pauses while a caller turn is open (`hatch.voice.turn_started/settled`).
- [ ] False-wake guard (S): after a wake, end the call as `no_speech` if nothing voiced is said.
  SDK: `linux/src/musegadget/voice_call.py` (speech_timeout).
- [ ] `hatch.voice.input_suppressed` (S): interrupt when Muse suppresses the current input.
  SDK: `voice_gate.py`.
- [ ] Muse repeating itself in a call: investigate with the `muse: call records` log line
  (record kinds and seconds of audio per reply).
- [ ] Full duplex trial (S): the Portal's DSP removes its own speaker from the mic; test talking
  over Muse with gating off.

## Voice and replies

- [ ] Spoken-style replies on voice turns (S): `"device":"mcu-wearable"` in the chat body.
  SDK: `link_client.py` send_chat/send_voice.
- [ ] Slow replies that come back later (M): after 20 s say "Muse is working on it", free the
  turn, and play the answer as its own turn when it lands (wake the screen, flash).
  SDK: `esp32/AGENTS.md` "Slow replies", `muse_chat_link.c`.
- [ ] Show Muse's messages outside our own turns (M): replies to things typed on the phone, late
  results. Today chat events with no open turn are dropped (`core/muse/session.go`).
  SDK: `linux/src/musegadget/terminal_chat.py`.
- [ ] Tap-to-answer options (M): `kind:"option"` widgets as a tappable list.
  SDK: `esp32/docs/reply-option-selection.md`.
- [ ] Reread the latest reply (S): a scrollable copy of the last answer.
- [ ] Resend voice notes that never arrived (S-M), with 5/15/30/60 s backoff.
  SDK: `muse_resend.c`.
- [ ] `announce` command (S, our own idea): Muse speaks and shows something unprompted, e.g. from
  a scheduled task.

## Device

- [ ] Register as a gadget with a usage guide (S): `device_family:"gadget"`, `model_id`,
  `metadata.model_name`, and a `device.guide` command returning up to 4 KB of Markdown about the
  Portal. Test first whether leaving `homehub` changes how Muse routes commands.
  SDK: `protocols/DEVICE.md`, `link_client.py`.
- [ ] `wakeword.disable` (S): Muse can turn the wake word off; only someone at the Portal turns
  it back on.
- [ ] Screen sleep and brightness (S): sleep cover, wake on reply.
- [ ] Apps Muse writes for the device (L, speculative): `app.run`/`app.install`, as a WebView.
  SDK: `esp32/components/muse_apps/`.

## Security

- [ ] Allow only Meta's hosts at pairing (S): `api.muse.ai`, `hatch-api.meta.ai`,
  `hatch.metaaivm.com`. SDK: `linux/src/musegadget/server_hosts.py`.
- [ ] Refuse compromised roots (S-M): bundle a root pool without emSign ECC G3 and Comodo AAA; the
  Portal's Android 10 trust store never updates. SDK: `linux/src/musegadget/tls.py`.

## Avatar

- [ ] Fetch the `connecting` clip and the dark-mode variants; use call portraits and the call idle
  video (with its display crop) during calls.
- [ ] Background colour from the avatar (`chat_theme_color`), breathing when idle, a bounce on a
  gesture other than tap.
