// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package cliprofile is the per-computer profile the wardyn CLI reads: the organisation's URL, so
// `wardyn run attach <id>` needs none on the command line. It holds no secret (mode 0644) and is written
// by `wardyn auth set-url` and by `wardyn-runnerd setup`.
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
	URL string `json:"url"`
}

// Dir is where the profile lives: the wardyn directory of the person's configuration directory.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "wardyn"), nil
}

// Read is the organisation URL in dir's profile, or "" when there is no profile or it names none.
func Read(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var p profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Join(dir, FileName), err)
	}
	return p.URL, nil
}

// Write replaces dir's profile with u, through a sibling file renamed into place so a reader never sees
// half of it.
func Write(dir, u string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(profile{URL: u})
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
