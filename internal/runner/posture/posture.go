// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package posture is what a runner reports about its device (design 0.9 §7.4).
// mdm_managed and disk_encrypted come from a root-owned file the device's MDM
// writes when one is present, else from the runner's own probe of the OS. Both
// are self-reported: they separate a lost or compromised laptop from a healthy
// one, and do not constrain a root developer.
package posture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxMDMFile bounds the MDM posture file.
const maxMDMFile = 4096

// Read is this device's posture now.
func Read() types.RunnerPosture { return read(mdmFile, 0) }

func read(path string, owner uint32) types.RunnerPosture {
	p := types.RunnerPosture{OS: runtime.GOOS, OSVersion: osVersion(), Evidence: types.PostureEvidenceOSProbe, DiskEncrypted: diskEncrypted()}
	if f, err := fromMDMFile(path, owner); err == nil {
		warnIgnored("", nil) // a usable file: the same break later is news again
		p.MDMManaged, p.DiskEncrypted, p.Evidence = f.MDMManaged, f.DiskEncrypted, types.PostureEvidenceMDMFile
	} else if !errors.Is(err, os.ErrNotExist) {
		warnIgnored(path, err)
	}
	return p
}

// lastIgnored is the reason last logged for an unusable MDM file. The runner reads posture at every
// connect and every 15 minutes, so the same reason is logged once, and again when it changes.
var lastIgnored struct {
	sync.Mutex
	reason string
}

func warnIgnored(path string, err error) {
	reason := ""
	if err != nil {
		reason = path + ": " + err.Error()
	}
	lastIgnored.Lock()
	repeat := reason != "" && lastIgnored.reason == reason
	lastIgnored.reason = reason
	lastIgnored.Unlock()
	if err != nil && !repeat {
		slog.Warn("runner posture: the MDM posture file is ignored", "path", path, "error", err)
	}
}

// mdmPosture is the file's closed shape.
type mdmPosture struct {
	MDMManaged    bool `json:"mdm_managed"`
	DiskEncrypted bool `json:"disk_encrypted"`
}

// fromMDMFile trusts only a regular file owned by owner (root in production)
// that nobody else can write.
func fromMDMFile(path string, owner uint32) (mdmPosture, error) {
	var out mdmPosture
	// One open file for the checks and the read, so the file judged is the file read; a link is not followed.
	// O_NONBLOCK: a FIFO opens at once (and is then refused as not a regular file) instead of waiting for a writer.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return out, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return out, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return out, errors.New("not a regular file")
	case !ok || st.Uid != owner:
		return out, errors.New("not owned by root")
	case fi.Mode().Perm()&0o022 != 0:
		return out, errors.New("group- or world-writable")
	case fi.Size() > maxMDMFile:
		return out, errors.New("too large")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxMDMFile+1))
	if err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return out, errors.New("trailing data")
	}
	return out, nil
}

// Reporter sends the `posture` event at READY and again when it changes.
type Reporter struct {
	read func() types.RunnerPosture

	mu       sync.Mutex
	last     types.RunnerPosture
	watching *runnerwire.Peer
}

// NewReporter reads this device.
func NewReporter() *Reporter { return &Reporter{read: Read} }

// Send reports the current posture on peer.
func (r *Reporter) Send(ctx context.Context, peer *runnerwire.Peer) error {
	return r.send(ctx, peer, r.read())
}

func (r *Reporter) send(ctx context.Context, peer *runnerwire.Peer, p types.RunnerPosture) error {
	data, err := runnerwire.Marshal(p)
	if err != nil {
		return err
	}
	if err := peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventPosture, Data: data}); err != nil {
		return err
	}
	r.mu.Lock()
	r.last = p
	r.mu.Unlock()
	return nil
}

// Watch re-reads the posture every interval until peer ends, and reports a change.
func (r *Reporter) Watch(peer *runnerwire.Peer, every time.Duration) {
	r.mu.Lock()
	if r.watching == peer {
		r.mu.Unlock()
		return
	}
	r.watching = peer
	r.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-peer.Done():
			return
		case <-t.C:
		}
		p := r.read()
		r.mu.Lock()
		changed := p != r.last
		r.mu.Unlock()
		if changed {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := r.send(ctx, peer, p); err != nil {
				slog.Warn("runner posture: report failed", "error", err)
			}
			cancel()
		}
	}
}
