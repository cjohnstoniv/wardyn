// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package runneridentity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/cjohnstoniv/wardyn/internal/federation"
)

// Pointer is a runner's local attach pointer file, <UserConfigDir>/wardyn/local-attach/<runner_id>.json:
// how the CLI finds that runner's attach socket, and which organisation the runner belongs to. runnerd
// writes it 0600 when its socket opens and removes it when the socket closes (design ATTACH §3.5.1).
type Pointer struct {
	RunnerID string `json:"runner_id"`
	OrgURL   string `json:"org_url"`
	Socket   string `json:"socket"`
}

// maxPointerBytes bounds the read: the file is three short JSON strings.
const maxPointerBytes = 4096

// geteuid is os.Geteuid; a seam so a test can stand in for another user.
var geteuid = os.Geteuid

// PointerDir is where runnerd writes its pointer files.
func PointerDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "wardyn", "local-attach"), nil
}

// ReadPointer reads runnerID's pointer file from dir. It reads only that file and never the runner's key.
// The directory must be 0700 and this user's, the file a regular 0600 file of this user's, and the socket
// it names a socket of this user's: anything else is refused, so another user cannot point this CLI at a
// socket of theirs.
func ReadPointer(dir, runnerID string) (Pointer, error) {
	if err := owned(dir, fs.ModeDir); err != nil {
		return Pointer{}, err
	}
	path := filepath.Join(dir, runnerID+".json")
	if err := owned(path, 0); err != nil {
		return Pointer{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Pointer{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPointerBytes+1))
	if err != nil {
		return Pointer{}, err
	}
	if len(b) > maxPointerBytes {
		return Pointer{}, fmt.Errorf("%s is over %d bytes", path, maxPointerBytes)
	}
	var p Pointer
	if err := json.Unmarshal(b, &p); err != nil {
		return Pointer{}, fmt.Errorf("%s: %w", path, err)
	}
	if p.RunnerID != runnerID {
		return Pointer{}, fmt.Errorf("%s names runner %q", path, p.RunnerID)
	}
	if p.Socket == "" {
		return Pointer{}, fmt.Errorf("%s names no socket", path)
	}
	if err := owned(p.Socket, fs.ModeSocket); err != nil {
		return Pointer{}, err
	}
	return p, nil
}

// ForOrg reports whether the pointer's runner belongs to the organisation at orgURL, compared in the
// normalised form the runner's identity binds (origin plus base path).
func (p Pointer) ForOrg(orgURL string) bool {
	return federation.OrgURLSHA256(p.OrgURL) == federation.OrgURLSHA256(orgURL)
}

// owned checks, without following a link, that path has type typ (0 for a regular file), belongs to this
// user, and gives no access to anyone else. A socket's mode is not checked: connecting needs write on it,
// and the parent directories already keep others out.
func owned(path string, typ fs.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Type() != typ {
		return fmt.Errorf("%s is not a %s", path, typeName(typ))
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the owner of " + path)
	}
	if int64(st.Uid) != int64(geteuid()) {
		return fmt.Errorf("%s belongs to another user", path)
	}
	if typ != fs.ModeSocket && fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is open to other users (mode %o)", path, fi.Mode().Perm())
	}
	return nil
}

func typeName(typ fs.FileMode) string {
	switch typ {
	case fs.ModeDir:
		return "directory"
	case fs.ModeSocket:
		return "socket"
	}
	return "regular file"
}
