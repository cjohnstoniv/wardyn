// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"strings"
)

// The person-facing sentences of a macOS VM's folder shares, in one place: they are the approved copy,
// word for word, and change only with it.

// MacVMNoFolderShare refuses a macOS VM run that shares a folder on a runner whose version cannot.
const MacVMNoFolderShare = "This runner's version can't share a folder into a macOS VM. Update the runner, or run without the folder."

// MacVMShareTarget refuses a share whose target a macOS guest cannot take.
const MacVMShareTarget = "A macOS VM shares a folder only from a path inside /home/agent (not /home/agent itself, and not under /home/agent/.wardyn). In the VM it appears under /Users/agent."

// MacVMFolderUnreadable refuses a share the runner could not read in time: a privacy denial, an
// unanswered prompt, or a slow volume. name is the folder's last path segment, never its full path.
func MacVMFolderUnreadable(name string) string {
	return fmt.Sprintf("Wardyn couldn't read %s on that Mac. If macOS is blocking it, allow the Wardyn runner under System Settings → Privacy & Security → Files and Folders, then start the run again.", name)
}

// MacVMDiskLocked refuses a run whose disk drive name another macOS VM on this Mac has attached
// (MAC-D04): a disk drive is in one run at a time. The VM of a run that has just ended holds its disk
// for about half a minute more while it shuts down, and the run already shows as ended, so the
// sentence names that case too.
func MacVMDiskLocked(name string) string { return name + macVMDiskLockedTail }

const macVMDiskLockedTail = " is in another macOS VM on this Mac, or is still being released by a run that just ended. " +
	"A disk drive is in one run at a time: try again in a minute, or when that run ends."

// macVMDiskLockedTailBefore is the sentence's tail from a runner older than this one.
const macVMDiskLockedTailBefore = " is in another macOS VM on this Mac. A disk drive is in one run at a time: try again when that run ends."

// IsMacVMDiskLocked reports whether msg is a MacVMDiskLocked sentence, this runner's or an older one's,
// so the run's failure shows it as written.
func IsMacVMDiskLocked(msg string) bool {
	return strings.HasSuffix(msg, macVMDiskLockedTail) || strings.HasSuffix(msg, macVMDiskLockedTailBefore)
}
