package heymuse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pgodlews/hey-muse-portal/core/muse"
)

// voiceProbe is what ProbeVoice found out, without any secret or the reply's words.
type voiceProbe struct {
	Online      bool            `json:"online"`
	AskErr      string          `json:"ask_err,omitempty"`
	ReplyChars  int             `json:"reply_chars"`
	ReplyIDs    int             `json:"reply_ids"`
	TTSPath     string          `json:"tts_path,omitempty"`
	TTSStatus   int             `json:"tts_status"`
	TTSBytes    int             `json:"tts_bytes"`
	TTSLooksMP3 bool            `json:"tts_looks_mp3"`
	TTSErr      string          `json:"tts_err,omitempty"`
	TTSFile     string          `json:"tts_file,omitempty"`
	Call        *muse.CallProbe `json:"call,omitempty"`
	TookSeconds float64         `json:"took_s"`
}

// ProbeVoice asks what this account's Muse allows for speech: a short question asked for a reply in
// voice, Muse's own MP3 of that reply (saved as muse-voice.mp3 in dir), and, if call is set, a
// realtime call started and at once ended. It blocks for up to two minutes and returns JSON.
func (a *App) ProbeVoice(dir string, call bool) string {
	started := time.Now()
	var out voiceProbe
	a.mu.Lock()
	r := a.client
	a.mu.Unlock()
	report := func() string {
		out.TookSeconds = time.Since(started).Round(100 * time.Millisecond).Seconds()
		b, _ := json.Marshal(out)
		return string(b)
	}
	if r == nil || r.c == nil {
		return report()
	}
	out.Online = r.c.State().Phase == muse.Online
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	reply, err := r.c.Ask(ctx, muse.SpokenText("Please say hello to the Portal in one short sentence."), nil)
	if err != nil {
		out.AskErr = err.Error()
	} else {
		out.ReplyChars, out.ReplyIDs = len(reply.Text), len(reply.MessageIDs)
		if n := len(reply.MessageIDs); n > 0 {
			path, status, mp3, err := r.c.SpeakReply(ctx, reply.MessageIDs[n-1])
			out.TTSPath, out.TTSStatus, out.TTSBytes = path, status, len(mp3)
			if err != nil {
				out.TTSErr = err.Error()
			}
			out.TTSLooksMP3 = len(mp3) > 3 && (string(mp3[:3]) == "ID3" || (mp3[0] == 0xFF && mp3[1]&0xE0 == 0xE0))
			if status >= 200 && status < 300 && len(mp3) > 0 {
				file := filepath.Join(dir, "muse-voice.mp3")
				if os.WriteFile(file, mp3, 0o600) == nil {
					out.TTSFile = file
				}
			}
		}
	}
	if call {
		p := r.c.ProbeCall(ctx)
		out.Call = &p
	}
	return report()
}
