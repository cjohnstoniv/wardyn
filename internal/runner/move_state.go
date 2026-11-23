// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// CarriedSet is what a move carries out of a paused run's sandbox: the roots it exports and the patterns
// it leaves out (movepack.Set).
type CarriedSet = movepack.Set

// CarriedManifest is a carried stream's manifest (movepack.Manifest): its entry count, file bytes, the
// files the exporter skipped and the digest the source, the relay and the destination each compute.
type CarriedManifest = movepack.Manifest

// StateExport is an export's answer. Stream is the carried files as a movepack stream; the caller reads it
// to its end and closes it. SpoolDrained says the run's spool on the source holds no record the
// organisation has not taken, so closing the run's spool cursor at commit loses none. A substrate that
// keeps no spool of its own answers true; a runner answers for its spool.
type StateExport struct {
	Stream       io.ReadCloser
	SpoolDrained bool
}

// StateExporter is an OPTIONAL Runner capability: read a stopped, kept sandbox's carried files. The
// exporter never follows a link: a symlink is carried as its text and a root that is one refuses. A
// hardlink is carried as a file, setuid, setgid and sticky bits are cleared, and sockets, fifos and devices
// are skipped and listed.
type StateExporter interface {
	ExportState(ctx context.Context, ref string, set CarriedSet) (StateExport, error)
}

// StateImporter is an OPTIONAL Runner capability: create a sandbox for spec with its agent not started,
// no proxy process and no proxy config (CheckImportSpec), and write carried, a movepack stream of set,
// beneath set's roots in it. maxBytes is the move's own bound on the stream (CheckCarriedLimit), the
// number the organisation's relay reads with too. The answer's manifest is what the importer read,
// computed by its own reader. An import that fails after the create removes what it created. ErrMoveCreate
// or ErrMoveFiles says which half failed.
type StateImporter interface {
	ImportSandbox(ctx context.Context, spec SandboxSpec, set CarriedSet, maxBytes int64, carried io.Reader) (Sandbox, CarriedManifest, error)
}

// MaxCarriedBytes is the largest bound a move may set on its carried stream: the 200 GiB that a move's
// max_carried_gib allows at most.
const MaxCarriedBytes int64 = 200 << 30

// CheckCarriedLimit refuses an import with no bound of its own: a missing or zero one, or one over
// MaxCarriedBytes. A reader never falls back to its default for a move.
func CheckCarriedLimit(maxBytes int64) error {
	if maxBytes <= 0 || maxBytes > MaxCarriedBytes {
		return fmt.Errorf("%w: %d bytes", errCarriedLimit, maxBytes)
	}
	return nil
}

// MoveImportPresenter is a marker: a substrate whose imported, unstarted sandbox its runner's observer
// reads present (its Status is not RunStopped with no exit code). A runner sends Caps.MoveImportPresent
// only when its substrate has it, so the organisation never moves a run to a runner that would then read
// the moved run's sandbox as gone. The Docker driver has it: a created container reads RunStarting.
type MoveImportPresenter interface {
	MoveImportReadsPresent()
}

// MoveUnbound tells a runner that the run it held was moved away: it leaves that sandbox out of the run
// state it reports. It is a cleanup hint only; the organisation never relies on it.
type MoveUnbound struct {
	RunID uuid.UUID `json:"run_id"`
	Ref   string    `json:"ref"`
}

// MoveUnbounder is an OPTIONAL Runner capability: pass a MoveUnbound notice to the runner that held the run.
type MoveUnbounder interface {
	MoveUnbound(ctx context.Context, n MoveUnbound) error
}

var (
	// ErrMoveCreate: the import failed creating the sandbox; nothing was written.
	ErrMoveCreate = errors.New("runner: the moved run's sandbox could not be created")
	// ErrMoveFiles: the import created the sandbox and failed writing the carried files into it.
	ErrMoveFiles = errors.New("runner: the carried files could not be written")

	errCarriedLimit        = errors.New("runner: an import's bound on the carried files must be from 1 byte to 200 GiB")
	errImportCarriesPolicy = errors.New("runner: an imported sandbox takes no proxy config; its policy comes only from the revive that starts it")
)

// CheckImportSpec refuses an import spec that carries any proxy config. The destination's policy is set
// only by the sealed revive that starts it after the move commits.
func CheckImportSpec(spec SandboxSpec) error {
	if !reflect.ValueOf(spec.ProxyConfig).IsZero() {
		return errImportCarriesPolicy
	}
	return nil
}
