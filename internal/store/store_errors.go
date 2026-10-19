// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrNotFound is returned when a Get* call finds no row.
var ErrNotFound = errors.New("store: not found")

// ErrConflict reports a fenced write losing its race: a source scan slot
// already claimed, or the requirements merge hitting the key cap.
var ErrConflict = errors.New("store: conflict")

// ErrDriveHomeNamespaceConflict is returned by UpsertUserDrive when a host_path
// drive would share a host_root with another host_path drive that derives home
// directory NAMES by a different rule. Wraps ErrConflict (as do
// ErrDriveSlugConflict and ErrDriveAllocated below) so callers asking only
// "is this a 409?" keep working, while the remedy stays distinguishable from a
// plain UNIQUE(name) collision.
var ErrDriveHomeNamespaceConflict = fmt.Errorf(
	"%w: another host_path drive on this host_root derives home directory names by a different rule", ErrConflict)

// ErrDriveSlugConflict is returned by UpsertUserDrive when a drive's name folds
// to a storage-object slug another drive already holds — "Corp NAS" against an
// existing "corp nas", or "Corp NAS (eng)" against "corp-nas-eng". Distinct
// from UNIQUE(name): the name itself is free, but types.DriveSlug(name) — the
// fragment every minted object name is built from — collides, so the remedy is
// a name differing by more than case or punctuation.
var ErrDriveSlugConflict = fmt.Errorf(
	"%w: another user drive's name folds to the same storage-object name", ErrConflict)

// ErrDriveAllocated is returned by UpsertUserDrive when the caller asked for the
// write to apply only while the drive has no allocations (refuseIfAllocated)
// and the row has some. It is the statement-level half of the API's re-home
// guard: since a plain read-then-write would let a grant created in between
// get silently re-homed, the precondition rides the writing statement under a
// FOR UPDATE on the drive row in the same transaction, making it race-free.
var ErrDriveAllocated = fmt.Errorf(
	"%w: this user drive gained an allocation while the write was being prepared", ErrConflict)

// ErrWorkspaceStale is returned by UpdateWorkspace when the row changed after
// the caller read it (every writer stamps updated_at), so replaying the
// caller's snapshot would revert a concurrent claim, record result or build.
// Wraps ErrConflict.
var ErrWorkspaceStale = fmt.Errorf(
	"%w: the workspace changed while this edit was being prepared", ErrConflict)

// ErrAlreadyDecided is returned when DecideApproval targets an approval that
// has already left PENDING. Fail closed: a second decision never overwrites
// the first.
var ErrAlreadyDecided = types.ErrApprovalAlreadyDecided

// ErrDuplicatePending is returned by CreateApproval when a partial unique index
// rejects a second open PENDING approval for the same dedup key (a concurrent
// raise lost the race). Callers treat it as a dedup signal — re-read and
// return the existing PENDING row — not a hard failure.
var ErrDuplicatePending = types.ErrDuplicatePendingApproval
