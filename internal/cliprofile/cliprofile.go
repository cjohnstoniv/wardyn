// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package cliprofile is the per-computer profile the wardyn CLI reads: the organisation's URL, so
// `wardyn attach <id>` needs none on the command line, and optionally the base URL its release downloads
// come from. It holds no secret (mode 0644) and is written by `wardyn login <url>` and by
// `wardyn-runnerd setup`. It also names, per organisation URL, which store holds this computer's CLI sign-in
// (`wardyn login`): only the store's kind, never the sign-in itself. Writing one field keeps the others.
package cliprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the profile's name inside Dir.
const FileName = "cli.json"

type profile struct {
	URL          string `json:"url"`
	ArtifactBase string `json:"artifact_base,omitempty"`
	// Sessions maps an organisation URL, as Normalize writes it, to the store holding its CLI sign-in
	// ("keychain" or "file").
	Sessions map[string]string `json:"sessions,omitempty"`
}

// Dir is where the profile lives: the wardyn directory of the person's configuration directory.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "wardyn"), nil
}

// load is dir's profile; a missing file is an empty profile.
func load(dir string) (profile, error) {
	var p profile
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return profile{}, fmt.Errorf("%s: %w", filepath.Join(dir, FileName), err)
	}
	return p, nil
}

// Read is the organisation URL in dir's profile, or "" when there is no profile or it names none.
func Read(dir string) (string, error) {
	p, err := load(dir)
	return p.URL, err
}

// ReadArtifactBase is the release download base in dir's profile, or "" when it names none.
func ReadArtifactBase(dir string) (string, error) {
	p, err := load(dir)
	return p.ArtifactBase, err
}

// Write sets dir's organisation URL to u and keeps the artifact base. A profile that cannot be parsed is
// replaced: there is nothing in it to keep.
func Write(dir, u string) error {
	p, _ := load(dir)
	p.URL = u
	return store(dir, p)
}

// WriteArtifactBase sets dir's artifact base to base, normalised like the URL and https only, and keeps
// the organisation URL.
func WriteArtifactBase(dir, base string) error {
	b, err := NormalizeArtifactBase(base)
	if err != nil {
		return err
	}
	p, _ := load(dir)
	p.ArtifactBase = b
	return store(dir, p)
}

// ReadSessionStore is the store holding the CLI sign-in for origin (a Normalize URL), or "" for none.
func ReadSessionStore(dir, origin string) (string, error) {
	p, err := load(dir)
	return p.Sessions[origin], err
}

// WriteSessionStore records that kind holds origin's CLI sign-in, or with kind "" that none does, and
// keeps every other field.
func WriteSessionStore(dir, origin, kind string) error {
	p, err := load(dir)
	if err != nil {
		return err
	}
	if kind == "" {
		delete(p.Sessions, origin)
	} else {
		if p.Sessions == nil {
			p.Sessions = map[string]string{}
		}
		p.Sessions[origin] = kind
	}
	return store(dir, p)
}

// store replaces dir's profile with p, through a sibling file renamed into place so a reader never sees
// half of it.
func store(dir string, p profile) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+FileName+".*")
	if err != nil {
		return err
	}
	_, werr := f.Write(append(body, '\n'))
	cerr := f.Chmod(0o644)
	if err := errors.Join(werr, cerr, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, FileName)); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return nil
}

// Normalize is raw as an organisation URL: scheme, host and base path, with no trailing slash, query,
// fragment or credentials. Whether the scheme is safe to send a token over is the caller's concern.
func Normalize(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("not a URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", fmt.Errorf("%q is not an organisation URL: it starts https://<host> (or http://<host> for a loopback one)", raw)
	}
	if u.User != nil {
		return "", errors.New("the organisation URL must not carry a user name or password")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// NormalizeArtifactBase is raw as a release download base: Normalize's form, and https only, because the
// CLI fetches a program from it.
func NormalizeArtifactBase(raw string) (string, error) {
	u, err := Normalize(raw)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("%q is not a download base: it starts https://<host>", raw)
	}
	return u, nil
}
