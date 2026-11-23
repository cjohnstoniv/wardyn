// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "errors"

// errSSHBackgroundOnly is the SSH door's fixed answer for a background task, as it read before the door asked
// any other refusal.
var errSSHBackgroundOnly = errors.New("ssh: this run is a background task; it has no SSH access")

// sshDoorRefusalErr is the error sshAuth returns for a door refusal: the fixed background-task error, or the
// refusal's own sentence (a Windows VM run's, which is desktop-only).
func sshDoorRefusalErr(rf *runRefusal) error {
	if rf.body.Reason == reasonRunBackgroundOnly {
		return errSSHBackgroundOnly
	}
	return errors.New("ssh: " + rf.body.Error)
}
