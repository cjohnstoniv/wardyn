// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

var (
	_ runner.StateExporter = (*Substrate)(nil)
	_ runner.StateImporter = (*Substrate)(nil)
	_ runner.MoveUnbounder = (*Substrate)(nil)
)

// errMoveNotOffered: the runner's caps lack MoveState, so it would refuse the call as unknown.
var errMoveNotOffered = errors.New("remote: this runner does not serve moves")

// moveCalls refuses a move call to a runner whose caps do not say it serves them.
func (s *Substrate) moveCalls() error {
	c, known := s.Caps()
	switch {
	case !known:
		return errCapsUnknown
	case !c.MoveState:
		return errMoveNotOffered
	}
	return nil
}

// ExportState asks the runner for the carried files of ref, a stopped, kept sandbox. The answer's Stream is
// the runner's move_export stream, a movepack stream the caller reads to its end and closes.
func (s *Substrate) ExportState(ctx context.Context, ref string, set runner.CarriedSet) (runner.StateExport, error) {
	if err := s.moveCalls(); err != nil {
		return runner.StateExport{}, err
	}
	if err := s.own(ref); err != nil {
		return runner.StateExport{}, err
	}
	sess, err := s.pin()
	if err != nil {
		return runner.StateExport{}, err
	}
	var res runnerwire.MoveExportResult
	if err := sess.Call(ctx, runnerwire.MethodMoveExport, runnerwire.MoveExportArgs{Ref: ref, Set: set}, &res); err != nil {
		return runner.StateExport{}, err
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		return runner.StateExport{}, err
	}
	if st.Kind() != runnerwire.KindMoveExport {
		_ = st.Close()
		return runner.StateExport{}, fmt.Errorf("runner answered move_export with a %q stream", st.Kind())
	}
	return runner.StateExport{Stream: st, SpoolDrained: res.SpoolDrained}, nil
}

// ImportSandbox has the runner create spec's sandbox, not started and with no proxy, and writes carried
// into it. It writes while it waits for the runner's end line, since the runner may end the import before
// it has read everything. ctx bounds the whole import. On an error the copy from carried may still be
// waiting on carried: the caller closes carried's source, which ends it.
func (s *Substrate) ImportSandbox(ctx context.Context, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	fail := func(stage, err error) (runner.Sandbox, runner.CarriedManifest, error) {
		return runner.Sandbox{}, runner.CarriedManifest{}, fmt.Errorf("%w: %w", stage, err)
	}
	// An import creates a sandbox, so it reads the Dispatch gate and refuses a resident value as a create does.
	if s.opts.Dispatch != nil {
		if err := s.opts.Dispatch(ctx); err != nil {
			return fail(runner.ErrMoveCreate, err)
		}
	}
	if err := s.moveCalls(); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	if err := runner.CheckImportSpec(spec); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	if err := runner.CheckCarriedLimit(maxBytes); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	if err := s.refuseResident(spec.RunID, spec); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	if err := s.checkClass(ctx, spec.ConfinementClass); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	sess, err := s.pin()
	if err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	var res runnerwire.StreamResult
	if err := sess.Call(ctx, runnerwire.MethodMoveImport, runnerwire.MoveImportArgs{Spec: spec, Set: set, MaxBytes: maxBytes}, &res); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		// The runner may have started the import: the stream it opened is reset, which ends it.
		return fail(runner.ErrMoveFiles, err)
	}
	defer st.Close()
	if st.Kind() != runnerwire.KindMoveImport {
		return fail(runner.ErrMoveFiles, fmt.Errorf("runner answered move_import with a %q stream", st.Kind()))
	}
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	wrote := make(chan error, 1)
	go func() {
		_, err := io.Copy(st, carried)
		if err == nil {
			err = st.CloseWrite()
		}
		wrote <- err
	}()
	end, err := readImportEnd(st)
	if err != nil {
		return fail(runner.ErrMoveFiles, fmt.Errorf("reading the runner's import outcome: %w", err))
	}
	if end.Error != nil {
		stage := runner.ErrMoveFiles
		if end.Stage == runnerwire.MoveImportStageCreate {
			stage = runner.ErrMoveCreate
		}
		return fail(stage, end.Error.Err())
	}
	if err := <-wrote; err != nil {
		return fail(runner.ErrMoveFiles, err)
	}
	if end.Sandbox == nil {
		return fail(runner.ErrMoveFiles, errors.New("the runner's import outcome names no sandbox"))
	}
	if err := s.own(end.Sandbox.Ref); err != nil {
		return fail(runner.ErrMoveFiles, err)
	}
	return *end.Sandbox, end.Manifest, nil
}

// readImportEnd reads the one line a move_import stream ends with.
func readImportEnd(st io.Reader) (runnerwire.MoveImportEnd, error) {
	var end runnerwire.MoveImportEnd
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			return end, runnerwire.Unmarshal(buf[:i], &end)
		}
		if len(buf) > runnerwire.MoveImportEndMaxBytes {
			return end, fmt.Errorf("the line is over %d bytes", runnerwire.MoveImportEndMaxBytes)
		}
		n, err := st.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil && bytes.IndexByte(buf, '\n') < 0 {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return end, err
		}
	}
}

// MoveUnbound tells the runner the run it held in n.Ref was moved away. It is a cleanup hint only.
func (s *Substrate) MoveUnbound(ctx context.Context, n runner.MoveUnbound) error {
	if err := s.moveCalls(); err != nil {
		return err
	}
	if err := s.own(n.Ref); err != nil {
		return err
	}
	return s.call(ctx, runnerwire.MethodMoveUnbound, n, nil)
}
