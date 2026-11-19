// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package rootfile reads an image admission trusted root file, confined to one directory. It is the one
// reader for both the site setting's check at write (internal/api) and the policy compile
// (internal/imageadmit), and it imports nothing beyond the standard library.
package rootfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Dir is the one directory a trusted root file may live in: under /etc/wardyn, where the compose stack mounts
// WARDYN_MANAGED_DIR (on Kubernetes, the chart's extraVolumes and extraVolumeMounts).
const Dir = "/etc/wardyn/sigstore"

// Max bounds a read: a real root is a few kilobytes.
const Max = 1 << 20

// errUnreadable is every failure to open or read the file. No error quotes the file.
var errUnreadable = errors.New("the file cannot be read")

// outside refuses a path outside dir.
func outside(dir string) error { return fmt.Errorf("the file must be in %s", dir) }

// name is path's name inside dir, rooted ("/name"), when the cleaned path is in dir. The rooted clean leaves
// no ".." in it, so dir joined with it is in dir.
func name(dir, path string) (string, bool) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(path) || !strings.HasPrefix(clean, dir+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Clean("/" + strings.TrimPrefix(clean, dir)), true
}

// Read returns the file path names, which must be in dir both as written (cleaned) and once its symbolic
// links are resolved. The file is opened through dir (os.Root), so no other file on the server is read, and
// at most Max bytes are kept: a larger file is refused.
func Read(dir, path string) ([]byte, error) {
	dir = filepath.Clean(dir)
	n, ok := name(dir, path)
	if !ok {
		return nil, outside(dir)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, errUnreadable
	}
	real, err := filepath.EvalSymlinks(filepath.Join(dir, n))
	if err != nil {
		return nil, errUnreadable
	}
	if n, ok = name(realDir, real); !ok {
		return nil, outside(dir)
	}
	d, err := os.OpenRoot(realDir)
	if err != nil {
		return nil, errUnreadable
	}
	defer d.Close()
	f, err := d.Open(strings.TrimPrefix(n, "/"))
	if err != nil {
		return nil, errUnreadable
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, Max+1))
	switch {
	case err != nil:
		return nil, errUnreadable
	case len(raw) > Max:
		return nil, fmt.Errorf("the file is larger than %d bytes", Max)
	}
	return raw, nil
}
