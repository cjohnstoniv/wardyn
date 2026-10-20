// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/federation"
)

// maxSunPath is the shortest sun_path limit runnerd runs under (macOS, 104 bytes with the NUL).
const maxSunPath = 103

// localSocket is the per-user local attach socket (design ATTACH §3.5.2). It is open only while the
// organisation's AUTH_OK allows local attach, and never under --local-attach=off. It grants nothing by
// itself: a connection gets a nonce, and a terminal only for a grant the org sends over the runner's
// authenticated stream.
type localSocket struct {
	runnerID, orgURL, stateDir, pointerDir string
	serve                                  func(net.Conn)
	// peerUID is the connecting process's uid from the kernel (SO_PEERCRED, LOCAL_PEERCRED).
	peerUID func(net.Conn) (uint32, error)

	mu        sync.Mutex
	ln        net.Listener
	sock, tmp string
}

// localPointer is <UserConfigDir>/wardyn/local-attach/<runner_id>.json: how the CLI finds this
// runner's socket, and which organisation it belongs to.
type localPointer struct {
	RunnerID string `json:"runner_id"`
	OrgURL   string `json:"org_url"`
	Socket   string `json:"socket"`
}

// newLocalSocket is nil under --local-attach=off: no socket, no pointer file, no capability.
func newLocalSocket(on bool, runnerID, orgURL, stateDir string, serve func(net.Conn)) (*localSocket, error) {
	if !on {
		return nil, nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("local attach pointer directory: %w", err)
	}
	return &localSocket{runnerID: runnerID, orgURL: federation.NormaliseOrgURL(orgURL), stateDir: stateDir,
		pointerDir: filepath.Join(cfg, "wardyn", "local-attach"), serve: serve, peerUID: socketPeerUID}, nil
}

// Set opens the socket when the organisation allows local attach and closes it when it does not. A
// socket that cannot open is logged: attach then goes through the organisation.
func (l *localSocket) Set(allow bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if allow == (l.ln != nil) {
		return
	}
	if !allow {
		l.closeLocked()
		return
	}
	if err := l.openLocked(); err != nil {
		l.closeLocked()
		slog.Warn("local attach socket did not open; attach goes through the organisation", "error", err)
	}
}

// Close removes the socket and the pointer file.
func (l *localSocket) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeLocked()
	return nil
}

func (l *localSocket) openLocked() error {
	path := filepath.Join(l.stateDir, "attach.sock")
	if len(path) > maxSunPath {
		dir, err := os.MkdirTemp(cmp.Or(os.Getenv("XDG_RUNTIME_DIR"), os.TempDir()), "wardyn-attach-") // 0700
		if err != nil {
			return err
		}
		l.tmp, path = dir, filepath.Join(dir, "attach.sock")
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil { // one an earlier runnerd left; the instance lock is ours
			return err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	l.ln, l.sock = ln, path
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if err := l.writePointer(); err != nil {
		return err
	}
	go l.accept(ln)
	return nil
}

func (l *localSocket) writePointer() error {
	if err := os.MkdirAll(l.pointerDir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(l.pointerDir); err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("local attach pointer directory %s must be a private directory", l.pointerDir)
	}
	b, err := json.Marshal(localPointer{RunnerID: l.runnerID, OrgURL: l.orgURL, Socket: l.sock})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(l.pointerDir, ".pointer-") // 0600
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(f.Name(), l.pointerPath())
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func (l *localSocket) pointerPath() string { return filepath.Join(l.pointerDir, l.runnerID+".json") }

func (l *localSocket) closeLocked() {
	if l.ln != nil {
		_ = l.ln.Close()
		_ = os.Remove(l.pointerPath())
	}
	if l.sock != "" {
		_ = os.Remove(l.sock)
	}
	if l.tmp != "" {
		_ = os.RemoveAll(l.tmp)
	}
	l.ln, l.sock, l.tmp = nil, "", ""
}

func (l *localSocket) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("local attach socket stopped accepting", "error", err)
			}
			return
		}
		go l.admit(c)
	}
}

// admit closes a connection from another uid before reading a byte of it.
func (l *localSocket) admit(c net.Conn) {
	uid, err := l.peerUID(c)
	if err != nil || int64(uid) != int64(os.Geteuid()) {
		_ = c.Close()
		return
	}
	l.serve(c)
}

// socketPeerUID reads the peer's uid from a unix connection.
func socketPeerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("local attach: not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var uerr error
	if err := raw.Control(func(fd uintptr) { uid, uerr = fdPeerUID(int(fd)) }); err != nil {
		return 0, err
	}
	return uid, uerr
}
