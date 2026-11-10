// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"context"
	"errors"
	"fmt"
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

// conflict is a runner pass's file that both sides changed (or that differs after a recovered
// record). The laptop's copy wins, but only once the sandbox's is kept on the laptop as a conflict
// copy. A pass that does not pull touches neither side; a pass back that cannot keep the copy
// touches neither side and counts the file as not brought back.
func (sy *Syncer) conflict(ctx context.Context, rel string, l syncFile, r os.FileInfo, rep *Report, pulled *int64) error {
	rep.Conflicts = append(rep.Conflicts, rel)
	if !sy.Opts.Pull {
		return nil
	}
	kept, err := sy.keepConflict(ctx, rel, r, rep, pulled)
	if !kept {
		rep.unsynced(rel)
		return err
	}
	return sy.push(ctx, rel, l, rep)
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
	if _, err := installDurable(sy.Root, tmp, tmpName, InstallNew, names...); err != nil {
		if errors.Is(err, errNamesTaken) {
			rep.refuse(rel, fmt.Sprintf("all %d conflict copy names are taken", maxConflictCopies))
			return false, nil
		}
		return false, sy.pathFailed(ctx, rep, rel, "keep the sandbox copy", err)
	}
	return true, nil
}
