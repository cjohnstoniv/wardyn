// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package posture is what a runner reports about its device (design 0.9 §7.4).
// mdm_managed and disk_encrypted come from a root-owned file the device's MDM
// writes when one is present, else (on a Mac) from the MDMManaged and DiskEncrypted
// keys of its managed preferences profile, else from the runner's own probe of the
// OS. Both are self-reported: they separate a lost or compromised laptop from a healthy
// one, and do not constrain a root developer.
package posture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/managedprefs"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxMDMFile bounds the MDM posture file.
const maxMDMFile = 4096

// The managed preferences profile, swapped in tests.
var (
	profileOS      = runtime.GOOS
	profileRoot    = "/Library/Managed Preferences"
	profileExtract = func(file, key string) (string, error) {
		return managedprefs.PlutilExtract("/usr/bin/plutil", file, key)
	}
)

// Read is this device's posture now.
func Read() types.RunnerPosture { return read(mdmFile, 0) }

func read(path string, owner uint32) types.RunnerPosture {
	p := types.RunnerPosture{OS: runtime.GOOS, OSVersion: osVersion(), Evidence: types.PostureEvidenceOSProbe, DiskEncrypted: diskEncrypted()}
	if f, err := fromMDMFile(path, owner); err == nil {
		warnIgnored("", nil) // a usable file: the same break later is news again
		p.MDMManaged, p.DiskEncrypted, p.Evidence = f.MDMManaged, f.DiskEncrypted, types.PostureEvidenceMDMFile
	} else if errors.Is(err, os.ErrNotExist) {
		// No file: the profile an MDM pushes is the other way it attests. A file that is there and unusable
		// is not worked around by it.
		if v, file, ok, perr := fromProfile(owner); perr != nil {
			warnIgnored(file, perr)
		} else if ok {
			warnIgnored("", nil)
			p.MDMManaged, p.Evidence = v.managed, types.PostureEvidenceMDMFile
			if v.diskSet {
				p.DiskEncrypted = v.disk
			}
		}
	} else {
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

// openTrusted opens a regular file owned by owner (root in production) that nobody else can write.
func openTrusted(path string, owner uint32) (*os.File, error) {
	// One open file for the checks and the read, so the file judged is the file read; a link is not followed.
	// O_NONBLOCK: a FIFO opens at once (and is then refused as not a regular file) instead of waiting for a writer.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		err = errors.New("not a regular file")
	case !ok || st.Uid != owner:
		err = errors.New("not owned by root")
	case fi.Mode().Perm()&0o022 != 0:
		err = errors.New("group- or world-writable")
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// fromMDMFile trusts only a regular file owned by owner (root in production)
// that nobody else can write.
func fromMDMFile(path string, owner uint32) (mdmPosture, error) {
	var out mdmPosture
	f, err := openTrusted(path, owner)
	if err != nil {
		return out, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return out, err
	} else if fi.Size() > maxMDMFile {
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

// profileValues are the posture keys of the managed preferences profile: MDMManaged, and DiskEncrypted when
// the profile sets it (diskSet), else the runner's own probe answers for it.
type profileValues struct{ managed, disk, diskSet bool }

// fromProfile is the posture the Mac's managed preferences profile (MDMManaged, DiskEncrypted) attests, and
// the profile file it came from; ok is false for no profile, or one that does not set MDMManaged. Only the
// device-wide file counts, never one under <root>/<user>/, which a person may be able to install for
// themselves. It is trusted on the terms of the MDM file: a regular file owned by owner that nobody else can
// write. Only a Mac has one.
func fromProfile(owner uint32) (v profileValues, file string, ok bool, err error) {
	if profileOS != "darwin" {
		return v, "", false, nil
	}
	files := managedprefs.Files(profileRoot, managedprefs.Domain)
	file = files[len(files)-1]
	f, err := openTrusted(file, owner)
	if errors.Is(err, os.ErrNotExist) {
		return v, "", false, nil
	}
	if err != nil {
		return v, file, false, err
	}
	f.Close()
	get := func(key string) (val, set bool, err error) {
		raw, err := profileExtract(file, key)
		if err != nil {
			return false, false, err
		}
		if val, set, err = managedprefs.Bool(raw); err != nil {
			err = fmt.Errorf("managed preference %s: %w", key, err)
		}
		return val, set, err
	}
	var set bool
	if v.managed, set, err = get("MDMManaged"); err != nil {
		return v, file, false, err
	} else if !set {
		return v, "", false, nil
	}
	if v.disk, v.diskSet, err = get("DiskEncrypted"); err != nil {
		return v, file, false, err
	}
	return v, file, true, nil
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
