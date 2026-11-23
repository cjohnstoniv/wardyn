// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// WindowsLicenceStore is the Windows VM licence pools, their holders, the registered Windows images and the
// live seats. The database holds the fence: a windows_vm run takes its seat in the statement that inserts it
// (migration 0401's take trigger), and frees it only by ending, so nothing here writes a seat. These writes
// refuse a change that would strand a live seat, naming the run that holds it (WindowsSeatLiveError).
type WindowsLicenceStore interface {
	ListWindowsLicencePools(ctx context.Context) ([]types.WindowsLicencePool, error)
	GetWindowsLicencePool(ctx context.Context, id uuid.UUID) (types.WindowsLicencePool, error)
	CreateWindowsLicencePool(ctx context.Context, p types.WindowsLicencePool) (types.WindowsLicencePool, error)
	// UpdateWindowsLicencePool renames the pool or changes its seats at revision; seats below the holder count
	// are refused (ErrWindowsLicenceSeatsBelowHolders).
	UpdateWindowsLicencePool(ctx context.Context, id uuid.UUID, revision int64, name *string, seats *int) (types.WindowsLicencePool, error)
	// DeleteWindowsLicencePool tombstones the pool, refused while a live seat is in it.
	DeleteWindowsLicencePool(ctx context.Context, id uuid.UUID) (types.WindowsLicencePool, error)

	ListWindowsLicenceHolders(ctx context.Context, poolID uuid.UUID) ([]types.WindowsLicenceHolder, error)
	// AddWindowsLicenceHolder gives subject a seat in the pool, refused once the pool's seats are all held
	// (ErrWindowsLicencePoolFull).
	AddWindowsLicenceHolder(ctx context.Context, poolID uuid.UUID, subject, by string) (types.WindowsLicenceHolder, error)
	// RemoveWindowsLicenceHolder takes subject's seat away, refused while a run holds it.
	RemoveWindowsLicenceHolder(ctx context.Context, poolID uuid.UUID, subject string) (types.WindowsLicenceHolder, error)
	// ListWindowsLicenceLeases is the pool's live seats.
	ListWindowsLicenceLeases(ctx context.Context, poolID uuid.UUID) ([]types.WindowsLicenceLease, error)

	ListWindowsImages(ctx context.Context) ([]types.WindowsImage, error)
	GetWindowsImage(ctx context.Context, id uuid.UUID) (types.WindowsImage, error)
	// RegisterWindowsImage registers a digest-pinned image bound to an active pool.
	RegisterWindowsImage(ctx context.Context, img types.WindowsImage) (types.WindowsImage, error)
	// UpdateWindowsImage renames the image or moves it to another pool at revision; a move is refused while a
	// run holds a seat through the image.
	UpdateWindowsImage(ctx context.Context, id uuid.UUID, revision int64, name *string, poolID *uuid.UUID) (types.WindowsImage, error)
	// RetireWindowsImage tombstones the image, refused while a run holds a seat through it.
	RetireWindowsImage(ctx context.Context, id uuid.UUID) (types.WindowsImage, error)

	// WindowsLaunchCheck reads what the take trigger will read for a run of ref created by subject, so a create
	// refuses with a clear reason before its insert. The trigger stays the fence.
	WindowsLaunchCheck(ctx context.Context, ref, subject string) (WindowsLaunch, error)
	// DeleteWindowsLicenceHoldersOf deletes every seat the subjects hold, refused, deleting nothing, while a run
	// holds one of them. It is the person erasure's half; the leaver purge deletes them in its own transaction.
	DeleteWindowsLicenceHoldersOf(ctx context.Context, subjects []string) (int64, error)
}

var _ WindowsLicenceStore = PG{}

// WindowsLaunch is what a launch of a registered Windows image by one person would take a seat in.
type WindowsLaunch struct {
	Image  types.WindowsImage
	Pool   types.WindowsLicencePool
	Holder types.WindowsLicenceHolder
}

// The refusals of the licence fence. Each take-trigger refusal arrives as SQLSTATE 23514 naming its constraint,
// which windowsInsertErr maps to one of these; WindowsLaunchCheck answers the same ones before the insert.
var (
	ErrWindowsVMsOff                   = errors.New("store: Windows VM sandboxes are turned off")
	ErrWindowsImageUnregistered        = errors.New("store: not a registered Windows image")
	ErrWindowsLicencePoolInactive      = errors.New("store: the Windows licence pool is not active")
	ErrWindowsLicenceNotHolder         = errors.New("store: no seat in the Windows licence pool")
	ErrWindowsLicenceInUse             = errors.New("store: the seat in the Windows licence pool is in use")
	ErrWindowsLicencePoolFull          = errors.New("store: every seat in the Windows licence pool is held")
	ErrWindowsLicenceSeatsBelowHolders = errors.New("store: fewer seats than the Windows licence pool's holders")
	ErrWindowsLicenceStale             = errors.New("store: the Windows licence pool or image revision is stale")
	ErrWindowsLicenceSeatLive          = errors.New("store: a run holds a seat in the Windows licence pool")
	// ErrWindowsVMFenced is a write a windows_vm run's fence refused: a run inserted already ended, or an update
	// of its kind, creator or image, or one that brings it back from an end. No route should reach it.
	ErrWindowsVMFenced = errors.New("store: the Windows VM run's licence fence refused the write")
)

// WindowsLicenceInUseError names the run that holds the seat; it is ErrWindowsLicenceInUse.
type WindowsLicenceInUseError struct{ RunID uuid.UUID }

func (e *WindowsLicenceInUseError) Error() string {
	return fmt.Sprintf("%v: run %s", ErrWindowsLicenceInUse, e.RunID)
}
func (e *WindowsLicenceInUseError) Unwrap() error { return ErrWindowsLicenceInUse }

// WindowsSeatLiveError names the run whose live seat refused a write; it is ErrWindowsLicenceSeatLive.
type WindowsSeatLiveError struct{ RunID uuid.UUID }

func (e *WindowsSeatLiveError) Error() string {
	return fmt.Sprintf("%v: run %s", ErrWindowsLicenceSeatLive, e.RunID)
}
func (e *WindowsSeatLiveError) Unwrap() error { return ErrWindowsLicenceSeatLive }
