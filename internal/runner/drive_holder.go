// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrDriveHolderUnknown is what a DriveHolderChecker answers for a backend it cannot judge. The caller refuses:
// "could not ask" never reads as "nothing holds it".
var ErrDriveHolderUnknown = errors.New("runner: this substrate cannot tell whether a sandbox holds this drive")

// DriveHolderChecker is an OPTIONAL Runner capability beside DriveReclaimer: the name of a sandbox that mounts the
// storage object mount names, or "" when none does. It reads only; it never creates or deletes anything.
type DriveHolderChecker interface {
	DriveHolder(ctx context.Context, mount types.DriveMount) (string, error)
}
