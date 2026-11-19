// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
)

// driveRootsOf is the drive roots in effect and where they came from, "" and nil with none.
func driveRootsOf(s doctorSettings) (roots []string, source string) {
	i := slices.IndexFunc(runnerFields, func(f runnerField) bool { return f.name == "drive_root" })
	f := runnerFields[i]
	r := s.resolved(f, "")
	if len(r.List) == 0 {
		return nil, ""
	}
	_, source = s.sourceOf(r, f, "", "drive_root")
	return r.List, source
}

// folderSharesStage says whether a folder or drive bound to this Mac can be shared into a macOS VM run:
// a drive root is set and the guest agent has Full Disk Access. It is never a failure, because many Macs
// share no folder and doctor exits non-zero on one. fda is "allowed", "refused" or "" (not read, or dropped
// by a tools update); adHoc is whether the tools record's agent_signature is ad_hoc. fdaReadAt is the edit that read it
// and toolsAt the last tools update, either zero when unknown. Only a guest agent signed ad hoc loses the
// grant at a tools update, so only then does a newer tools update make the reading doubtful.
func folderSharesStage(roots []string, rootsSource, fda string, adHoc bool, fdaReadAt, toolsAt time.Time) doctor.Stage {
	const name = "folder-shares"
	day := func(t time.Time) string { return t.Local().Format(time.DateOnly) }
	switch {
	case len(roots) == 0:
		return doctor.SkipStage(name, "no drive root: folders and drives bound to this Mac are refused. Set one: wardyn-runnerd setup --drive-root <dir>")
	case fda == "refused":
		return doctor.SkipStage(name, "Full Disk Access is not granted to the guest agent: a run with a shared folder stops within 10 seconds. Grant it in macos-vm edit-base.")
	case fda == "":
		return doctor.SkipStage(name, "Full Disk Access has not been read (no macos-vm edit-base since the last tools update, or the guest agent could not answer): grant and check it in macos-vm edit-base.")
	case fda != "allowed":
		return doctor.SkipStage(name, "Full Disk Access reads \""+fda+"\", which doctor does not know: check it in macos-vm edit-base.")
	case adHoc && !fdaReadAt.IsZero() && toolsAt.After(fdaReadAt):
		return doctor.SkipStage(name, "tools were updated after Full Disk Access was last read ("+day(fdaReadAt)+", tools "+day(toolsAt)+"); a guest agent signed ad hoc loses it at each tools update. Check it in macos-vm edit-base.")
	}
	read := "allowed (as the base last read it"
	if !fdaReadAt.IsZero() {
		read += ", " + day(fdaReadAt)
	}
	return doctor.OKStage(name, "drive roots "+strings.Join(roots, ", ")+" (from "+rootsSource+"); Full Disk Access for the guest agent "+read+"). The organisation's workspace roots are set on its server and cannot be read here.")
}
