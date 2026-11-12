// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// maxConflictCopies is how many conflict copy names one file may take in its directory.
const maxConflictCopies = 9

// unsynced counts rel among the sandbox's changes not brought back, once.
func (r *Report) unsynced(rel string) {
	k := rel + "\x00unsynced"
	if r.seenSet[k] {
		return
	}
	r.seenSet[k] = true
	r.Unsynced++
	r.UnsyncedPaths = append(r.UnsyncedPaths, rel)
}

// notPreserved marks the report not preserved, keeping the first reason.
func (r *Report) notPreserved(why string) {
	if r.NotPreserved == "" {
		r.NotPreserved = why
	}
}

// keptFailed is pathFailed for a change the laptop must keep. On a runner pass, a failure also
// counts rel as not brought back and marks the report not preserved, whether or not it ends the
// pass: one that does not keeps its refusal, so an older server still shows it, and one that does
// is still returned, for the pass to cut. `wardyn sync` keeps pathFailed alone.
func (sy *Syncer) keptFailed(ctx context.Context, rep *Report, rel, what string, err error) error {
	aerr := sy.pathFailed(ctx, rep, rel, what, err)
	if sy.Opts.RunnerPass {
		rep.unsynced(rel)
		rep.notPreserved(what + " " + rel + ": " + err.Error())
	}
	return aerr
}

// conflictNames are the names a sandbox copy of rel may take on the laptop, in order:
// "<base> (sandbox conflict <tag>)<ext>", then "... <tag> 2)<ext>" up to maxConflictCopies.
func conflictNames(rel, tag string) []string {
	dir, name := path.Split(rel)
	ext := path.Ext(name)
	if ext == name {
		ext = ""
	}
	base := strings.TrimSuffix(name, ext)
	names := make([]string, 0, maxConflictCopies)
	for i := 1; i <= maxConflictCopies; i++ {
		n := ""
		if i > 1 {
			n = fmt.Sprintf(" %d", i)
		}
		names = append(names, dir+base+" (sandbox conflict "+tag+n+")"+ext)
	}
	return names
}

// conflict is a runner pass's file that both sides changed, or that a pass after a recovered
// record finds on both. Two copies holding the same bytes are in step: once the laptop's file and
// its directories are synced they are recorded (recordKept), and nothing is copied or pushed.
// Otherwise the laptop's copy wins, but only once the sandbox's is kept on the laptop as a conflict
// copy. A pass that does not pull touches neither side; a pass
// back that cannot keep the copy touches neither side and counts the file as not brought back.
func (sy *Syncer) conflict(ctx context.Context, rel string, l syncFile, r os.FileInfo, rep *Report, pulled *int64) error {
	if rsum, ok := sy.remoteSum(rel, l.side.Size, r.Size()); ok {
		// The bytes compared are the bytes synced: an earlier write may be visible but not on the disk.
		kept, err := sy.recordKept(rel, remoteSide(r), func(f *os.File, fi os.FileInfo) bool {
			sum, ok := sumOf(f, fi.Size())
			return ok && fi.Size() == r.Size() && bytes.Equal(sum, rsum)
		})
		if err != nil {
			return sy.keptFailed(ctx, rep, rel, "keep the local file", err)
		}
		if kept {
			return nil
		}
	}
	rep.Conflicts = append(rep.Conflicts, rel)
	if !sy.Opts.Pull {
		return nil
	}
	kept, err := sy.keepConflict(ctx, rel, r, rep, pulled)
	if !kept {
		rep.unsynced(rel)
		if sy.Opts.RunnerPass {
			rep.notPreserved("keep the sandbox copy of " + rel + ": not kept")
		}
		return err
	}
	return sy.push(ctx, rel, l, rep)
}

// listedConflict is a conflict a pass only lists. A runner pass back keeps the sandbox's copy as a
// conflict copy (keepConflict); one it cannot keep is not brought back and not preserved.
func (sy *Syncer) listedConflict(ctx context.Context, rel string, r os.FileInfo, rep *Report, pulled *int64) error {
	rep.Conflicts = append(rep.Conflicts, rel)
	if !sy.Opts.RunnerPass || !sy.Opts.Pull {
		return nil
	}
	kept, err := sy.keepConflict(ctx, rel, r, rep, pulled)
	if !kept {
		rep.unsynced(rel)
		rep.notPreserved("keep the sandbox copy of " + rel + ": not kept")
	}
	return err
}

// remoteSum is the sha256 of the sandbox's rel, read only when both sides list the same size.
func (sy *Syncer) remoteSum(rel string, lsize, rsize int64) ([]byte, bool) {
	if lsize != rsize {
		return nil, false
	}
	f, err := sy.Client.Open(path.Join(sy.Opts.Remote, rel))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, rsize))
	return h.Sum(nil), err == nil && n == rsize
}

// keepConflict pulls the sandbox's rel to the first free conflict copy name, durably, never
// replacing a file; a name already holding the same bytes counts as kept. It reports whether the
// copy is kept and proven durable.
func (sy *Syncer) keepConflict(ctx context.Context, rel string, r os.FileInfo, rep *Report, pulled *int64) (bool, error) {
	names := conflictNames(rel, sy.Opts.RunTag)
	for _, n := range names {
		if syncNamesIgnoreFile(n) || sy.ig.orgDenied(n, false) || (sy.Opts.Credential != nil && sy.Opts.Credential(n)) {
			rep.refuse(rel, "the conflict copy's name is not allowed")
			return false, nil
		}
	}
	if err := syncCheckLocal(sy.Root, names[0]); err != nil {
		rep.refuse(rel, "keep the sandbox copy: "+err.Error())
		return false, nil
	}
	tmpName, tmp, err := sy.fetch(ctx, rel, names[0], r, 0o644, rep, pulled)
	if tmp == nil {
		return false, err
	}
	if _, err := installDurable(sy.Root, sy.dirs, tmp, tmpName, InstallNew, names...); err != nil {
		if errors.Is(err, errNamesTaken) {
			rep.refuse(rel, fmt.Sprintf("all %d conflict copy names are taken", maxConflictCopies))
			return false, nil
		}
		return false, sy.keptFailed(ctx, rep, rel, "keep the sandbox copy", err)
	}
	return true, nil
}
