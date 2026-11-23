// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Windows VM licence pools. An organisation declares how many Windows seats it holds (a pool) and who holds one
// (its holders), and registers the Windows images it supplies, each bound to one pool. A windows_vm run takes a
// seat (a lease) in the same statement that inserts it, and frees it only by ending. The pool records the
// organisation's own entitlement: it is not a licence, and the audit is the compliance record.

// Windows licence pool states. A deleted pool is kept as a tombstone, so the audit and an ended run still name it.
const (
	WindowsLicencePoolActive  = "active"
	WindowsLicencePoolDeleted = "deleted"
)

// Windows image states. A retired image is kept as a tombstone, and its ref may be registered again.
const (
	WindowsImageActive  = "active"
	WindowsImageRetired = "retired"
)

// WindowsLicenceSeatsMax is the most seats a pool may declare; every pool declares at least one.
const WindowsLicenceSeatsMax = 10000

// WindowsLicenceNameMax is the longest pool or image name, in characters.
const WindowsLicenceNameMax = 64

// WindowsLicencePool is one entitlement: at most Seats people hold a seat in it, so at most Seats of its
// Windows VMs run at once.
type WindowsLicencePool struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Seats int       `json:"seats"`
	State string    `json:"state"`
	// Revision starts at 1 and rises by one on every change to the pool.
	Revision  int64     `json:"revision"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks the pool's name and seat count.
func (p WindowsLicencePool) Validate() error {
	if err := validateWindowsLicenceName("pool", p.Name); err != nil {
		return err
	}
	return ValidateWindowsLicenceSeats(p.Seats)
}

// ValidateWindowsLicenceSeats accepts 1 to WindowsLicenceSeatsMax seats.
func ValidateWindowsLicenceSeats(seats int) error {
	if seats < 1 || seats > WindowsLicenceSeatsMax {
		return fmt.Errorf("a Windows licence pool has 1 to %d seats", WindowsLicenceSeatsMax)
	}
	return nil
}

// WindowsLicenceHolder is one person who holds a seat in a pool. Subject is the person's subject, stored
// lower-cased and trimmed (NormalizeWindowsLicenceSubject), and a person holds at most one seat per pool.
type WindowsLicenceHolder struct {
	ID      uuid.UUID `json:"id"`
	PoolID  uuid.UUID `json:"pool_id"`
	Subject string    `json:"subject"`
	AddedBy string    `json:"added_by"`
	AddedAt time.Time `json:"added_at"`
}

// NormalizeWindowsLicenceSubject is the stored form of a holder's subject: lower-cased, with leading and
// trailing spaces removed, as the database's lower(btrim(...)) gives it.
func NormalizeWindowsLicenceSubject(subject string) string {
	return strings.ToLower(strings.Trim(subject, " "))
}

// WindowsImage is a Windows image the organisation registered: an OCI image holding one qcow2 disk, named by
// its digest, and bound to the pool whose seats its runs take.
type WindowsImage struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Ref           string    `json:"ref"`
	LicencePoolID uuid.UUID `json:"licence_pool_id"`
	State         string    `json:"state"`
	Revision      int64     `json:"revision"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Validate checks the image's name, its digest-pinned ref and that it names a pool.
func (i WindowsImage) Validate() error {
	if err := validateWindowsLicenceName("image", i.Name); err != nil {
		return err
	}
	if err := ValidateWindowsImageRef(i.Ref); err != nil {
		return err
	}
	if i.LicencePoolID == uuid.Nil {
		return errors.New("a Windows image names the licence pool its runs take a seat in")
	}
	return nil
}

// windowsImageRef is a digest-pinned image reference: a repository, then @sha256: and 64 lower-case hex digits.
var windowsImageRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// ValidateWindowsImageRef accepts only a digest-pinned ref, so the image a run boots is the image the
// organisation registered: a tag can move, a digest cannot.
func ValidateWindowsImageRef(ref string) error {
	if !windowsImageRef.MatchString(ref) {
		return errors.New("a Windows image ref is pinned by digest: <repository>@sha256:<64 hex digits>")
	}
	return nil
}

// WindowsLicenceLease is one live seat: the run that holds it, the pool, the holder and the image. The
// database writes and removes leases itself, when a windows_vm run is inserted and when it ends.
type WindowsLicenceLease struct {
	RunID    uuid.UUID `json:"run_id"`
	PoolID   uuid.UUID `json:"pool_id"`
	HolderID uuid.UUID `json:"holder_id"`
	ImageID  uuid.UUID `json:"image_id"`
	TakenAt  time.Time `json:"taken_at"`
}

// validateWindowsLicenceName accepts a printable, trimmed, single-line name of 1 to WindowsLicenceNameMax characters.
func validateWindowsLicenceName(what, name string) error {
	if name == "" || name != strings.TrimSpace(name) {
		return fmt.Errorf("name a Windows %s in 1 to %d characters, without leading or trailing spaces", what, WindowsLicenceNameMax)
	}
	n := 0
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("a Windows %s name is one printable line", what)
		}
		n++
	}
	if n > WindowsLicenceNameMax {
		return fmt.Errorf("a Windows %s name is at most %d characters", what, WindowsLicenceNameMax)
	}
	return nil
}
