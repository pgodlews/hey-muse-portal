// The paired Muse's identity and files: its name and avatar, read over the device's session. This
// follows esp32/main/avatar_sync.c, avatar_identity.c, avatar_clip_refs.c and vm_file_path.h in
// Meta's Muse Gadget SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the
// Apache License, Version 2.0.

package muse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AvatarClips are the phone app's animation families the identity may name, in the SDK's order.
var AvatarClips = []string{"idle", "waking", "working", "making_something", "waiting_for_subagents", "milestone_level_up"}

// MuseIdentity is who the paired Muse is, as far as a gadget is told.
type MuseIdentity struct {
	Name   string
	Avatar struct {
		ImageExists bool   `json:"image_exists"`
		ImagePath   string `json:"image_path"`
		Revision    string `json:"assets_revision"`
		// Clips maps an animation family (AvatarClips) to the path of its MP4.
		Clips map[string]string `json:"video_variants"`
	}
	// Raw is the whole result, for finding out what else a Muse says about itself.
	Raw json.RawMessage
}

// MuseIdentity asks Muse who it is: its name and its avatar's files.
func (c *Client) MuseIdentity(ctx context.Context) (MuseIdentity, error) {
	var id MuseIdentity
	s, err := c.online()
	if err != nil {
		return id, err
	}
	r := &request{done: make(chan struct{})}
	stream, err := s.open(frame{
		kind: frameRequest, verb: http.MethodGet, path: "/identity",
		headers: []header{{"x-request-id", uuid4()}, {"x-app-id", appID}}, endBody: true,
	}, r)
	if err != nil {
		return id, err
	}
	defer s.forget(stream)
	if err := s.await(ctx, r, 20*time.Second); err != nil {
		return id, err
	}
	if r.status/100 != 2 {
		return id, fmt.Errorf("muse: identity: HTTP %d", r.status)
	}
	var body struct {
		Result struct {
			Name   string          `json:"name"`
			Avatar json.RawMessage `json:"avatar"`
		} `json:"result"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		return id, fmt.Errorf("muse: identity: %w", err)
	}
	id.Name = body.Result.Name
	var whole struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(r.body, &whole) == nil {
		id.Raw = whole.Result
	}
	if len(body.Result.Avatar) > 0 {
		// A clip that is null or not a string is left out rather than failing the rest.
		var raw struct {
			ImageExists bool           `json:"image_exists"`
			ImagePath   string         `json:"image_path"`
			Revision    string         `json:"assets_revision"`
			Clips       map[string]any `json:"video_variants"`
		}
		if json.Unmarshal(body.Result.Avatar, &raw) == nil {
			id.Avatar.ImageExists, id.Avatar.ImagePath, id.Avatar.Revision = raw.ImageExists, raw.ImagePath, raw.Revision
			id.Avatar.Clips = map[string]string{}
			for k, v := range raw.Clips {
				if p, ok := v.(string); ok && p != "" && len(p) < 512 {
					id.Avatar.Clips[k] = p
				}
			}
		}
	}
	return id, nil
}

// fileChunk is how much of a file one request asks for.
const fileChunk = 256 << 10

// ReadFile reads a file of the paired Muse's, at most limit bytes, in ranges.
func (c *Client) ReadFile(ctx context.Context, path string, limit int) ([]byte, error) {
	s, err := c.online()
	if err != nil {
		return nil, err
	}
	url := filePath(path)
	var out []byte
	for {
		want := min(fileChunk, limit+1-len(out))
		r := &request{done: make(chan struct{})}
		stream, err := s.open(frame{
			kind: frameRequest, verb: http.MethodGet, path: url,
			headers: []header{
				{"Range", fmt.Sprintf("bytes=%d-%d", len(out), len(out)+want-1)},
				{"x-request-id", uuid4()},
				{"x-app-id", appID},
			},
			endBody: true,
		}, r)
		if err != nil {
			return nil, err
		}
		err = s.await(ctx, r, 30*time.Second)
		s.forget(stream)
		if err != nil {
			return nil, err
		}
		switch {
		case r.status == http.StatusOK && len(out) == 0:
			// The whole file, ranges or not.
			if len(r.body) > limit {
				return nil, errors.New("muse: the file is too big")
			}
			return r.body, nil
		case r.status == http.StatusPartialContent:
			out = append(out, r.body...)
			if len(out) > limit {
				return nil, errors.New("muse: the file is too big")
			}
			if len(r.body) < want || len(r.body) == 0 {
				return out, nil
			}
		case r.status == http.StatusRequestedRangeNotSatisfiable && len(out) > 0:
			// The last range ended exactly at the end.
			return out, nil
		default:
			return nil, fmt.Errorf("muse: reading a file: HTTP %d", r.status)
		}
	}
}

// filePath is a VM path as an /fs/raw request, escaped as a path and never as a query.
func filePath(path string) string {
	path = strings.TrimLeft(path, "/")
	var b strings.Builder
	b.WriteString("/fs/raw/")
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Schema describes JSON by its shape alone: every key with the type of its value, and strings only
// as a length and an extension, so nothing of the owner's leaks into a log.
func Schema(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "?"
	}
	var b strings.Builder
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(prefix+"."+k, t[k])
			}
		case []any:
			fmt.Fprintf(&b, "%s=array[%d] ", prefix, len(t))
			if len(t) > 0 {
				walk(prefix+"[0]", t[0])
			}
		case string:
			ext := ""
			if i := strings.LastIndexByte(t, '.'); i >= 0 && len(t)-i <= 6 && !strings.ContainsAny(t[i:], "/ ") {
				ext = t[i:]
			}
			fmt.Fprintf(&b, "%s=str%s ", prefix, ext)
		case nil:
			fmt.Fprintf(&b, "%s=null ", prefix)
		default:
			fmt.Fprintf(&b, "%s=%T ", prefix, t)
		}
	}
	walk("", v)
	return b.String()
}
