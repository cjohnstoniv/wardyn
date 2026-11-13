// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"fmt"
)

// syncGuest asks the guest to write the drive to disk, and says whether it confirmed.
func (j driveOpenJob) syncGuest() error {
	ctx, cancel := context.WithTimeout(j.hard, driveOpenSyncWait)
	defer cancel()
	return j.run(ctx, nil, "/bin/sync")
}

// syncOnReturn is deferred by driveOpenWork: a guest that has the drive is synced before the caller ends
// the VM, unless a write back already tried to (*tried), whose failure was returned to the person.
func (j driveOpenJob) syncOnReturn(tried *bool) {
	if *tried {
		return
	}
	if err := j.syncGuest(); err != nil {
		fmt.Fprintf(j.out, "The throwaway VM did not confirm it had written the drive (%s).\n", driveOpenSafe(err.Error()))
	}
}

// confirmWriteBack syncs the guest after a write back sent its changes, so that "written back" is only said
// once the guest confirmed them. A failed sync is an error: the folder holds the only confirmed copy.
func (j driveOpenJob) confirmWriteBack(tried *bool) error {
	*tried = true
	if err := j.syncGuest(); err != nil {
		return fmt.Errorf("the throwaway VM did not confirm it had written the changes: %w. The changes were sent but are not confirmed", err)
	}
	return nil
}
