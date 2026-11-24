package heymuse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/pgodlews/hey-muse-portal/core/muse"
)

// avatarManifest is what SyncAvatar keeps beside the files: the owner's own Muse, never published.
type avatarManifest struct {
	Name     string            `json:"name"`
	Revision string            `json:"revision"`
	Source   string            `json:"source"` // a hash of the paths, for a Muse with no revision
	Still    string            `json:"still,omitempty"`
	Clips    map[string]string `json:"clips,omitempty"`
	Synced   int64             `json:"synced"`
}

const (
	maxAvatarStill = 2 << 20
	maxAvatarClip  = 24 << 20
)

// SyncAvatar fetches the paired Muse's name and avatar into dir: its still image and the phone
// app's animation clips, kept until Muse says they changed. It returns the manifest as JSON (name,
// still and clips as file names in dir), or "" when Muse could not be asked. It blocks; call it
// off the main thread.
func (a *App) SyncAvatar(dir string) string {
	a.mu.Lock()
	r := a.client
	a.mu.Unlock()
	if r == nil || r.c == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id, err := r.c.MuseIdentity(ctx)
	if err != nil {
		slog.Warn("muse: no identity", "err", err)
		return ""
	}
	slog.Info("muse: identity shape", "schema", muse.Schema(id.Raw))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(id.Avatar.ImagePath))
	for _, k := range muse.AvatarClips {
		h.Write([]byte{0})
		h.Write([]byte(id.Avatar.Clips[k]))
	}
	source := hex.EncodeToString(h.Sum(nil))[:16]

	manifestPath := filepath.Join(dir, "manifest.json")
	var old avatarManifest
	if b, err := os.ReadFile(manifestPath); err == nil {
		_ = json.Unmarshal(b, &old)
	}
	m := avatarManifest{Name: id.Name, Revision: id.Avatar.Revision, Source: source, Clips: map[string]string{}, Synced: time.Now().Unix()}
	unchanged := old.Source == source && old.Revision == id.Avatar.Revision && id.Avatar.Revision != ""
	keep := func(name string) bool {
		if !unchanged || name == "" {
			return false
		}
		st, err := os.Stat(filepath.Join(dir, name))
		return err == nil && st.Size() > 0
	}
	fetch := func(path, name string, limit int) string {
		if keep(name) {
			return name
		}
		start := time.Now()
		b, err := r.c.ReadFile(ctx, path, limit)
		if err != nil || len(b) == 0 {
			slog.Warn("muse: an avatar file did not come", "file", name, "err", err)
			return ""
		}
		tmp := filepath.Join(dir, name+".tmp")
		if os.WriteFile(tmp, b, 0o600) != nil || os.Rename(tmp, filepath.Join(dir, name)) != nil {
			return ""
		}
		slog.Info("muse: avatar file", "file", name, "bytes", len(b), "ms", time.Since(start).Milliseconds())
		return name
	}
	if id.Avatar.ImageExists && id.Avatar.ImagePath != "" {
		m.Still = fetch(id.Avatar.ImagePath, "still"+ext(id.Avatar.ImagePath, ".webp"), maxAvatarStill)
	}
	for _, k := range muse.AvatarClips {
		if p := id.Avatar.Clips[k]; p != "" {
			if name := fetch(p, k+ext(p, ".mp4"), maxAvatarClip); name != "" {
				m.Clips[k] = name
			}
		}
	}
	// Only what is named in the identity is shown in the log: the paths are the owner's.
	slog.Info("muse: avatar synced", "has_name", id.Name != "", "still", m.Still != "", "clips", len(m.Clips),
		"named_clips", len(id.Avatar.Clips), "unchanged", unchanged)
	b, _ := json.Marshal(m)
	_ = os.WriteFile(manifestPath, b, 0o600)
	a.setAgentName(id.Name)
	return string(b)
}

// ext is the path's extension, or def when it has none or an odd one.
func ext(path, def string) string {
	e := filepath.Ext(path)
	switch e {
	case ".webp", ".png", ".jpg", ".jpeg", ".mp4", ".webm", ".gif":
		return e
	}
	return def
}
