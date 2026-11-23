// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// move serves the three move calls: the substrate's export and import, and the unbound notice.
func (d *Dispatcher) move(ctx context.Context, peer *runnerwire.Peer, id uint32, method string, args json.RawMessage) (any, error) {
	switch method {
	case runnerwire.MethodMoveExport:
		return d.moveExport(ctx, peer, id, args)
	case runnerwire.MethodMoveImport:
		return d.moveImport(ctx, peer, id, args)
	}
	return d.moveUnbound(ctx, args)
}

// moveExport opens a move_export stream and copies the substrate's export of a stopped, kept sandbox
// this runner holds onto it. It changes nothing on the sandbox.
func (d *Dispatcher) moveExport(ctx context.Context, peer *runnerwire.Peer, id uint32, args json.RawMessage) (any, error) {
	var a runnerwire.MoveExportArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	ref, err := d.local(a.Ref)
	if err != nil {
		return nil, err
	}
	if err := d.owned(ctx, ref); err != nil {
		return nil, err
	}
	exp, ok := d.cfg.Substrate.(runner.StateExporter)
	if !ok {
		return nil, runnerwire.Refuse("this runner cannot export a run's files")
	}
	// Read before the export: the sandbox is stopped, so the run's spool takes no new decision meanwhile.
	drained := false
	if d.cfg.SpoolDrained != nil {
		if drained, err = d.cfg.SpoolDrained(ctx, ref); err != nil {
			return nil, err
		}
	}
	stream, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindMoveExport, Target: a.Ref, Call: id})
	if err != nil {
		return nil, err
	}
	// The export lives as long as its stream, not the call.
	out, err := exp.ExportState(stream.Context(), ref, a.Set)
	if err != nil {
		_ = stream.Reset(runnerwire.ResetInternal)
		return nil, err
	}
	go func() {
		_, err := io.Copy(stream, out.Stream)
		if err = errors.Join(err, out.Stream.Close()); err != nil {
			slog.Warn("runner: a move's export ended early", slog.String("ref", a.Ref), slog.String("error", err.Error()))
			_ = stream.Reset(runnerwire.ResetInternal)
			return
		}
		_ = stream.CloseWrite()
	}()
	return runnerwire.MoveExportResult{Stream: stream.ID(), SpoolDrained: drained && out.SpoolDrained}, nil
}

// moveImport takes a moved run's sandbox: it records the creation intent as a create does, opens a
// move_import stream, and imports what the org writes on it. The outcome is the stream's one end line.
func (d *Dispatcher) moveImport(ctx context.Context, peer *runnerwire.Peer, id uint32, args json.RawMessage) (any, error) {
	var a runnerwire.MoveImportArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	spec := a.Spec
	if err := spec.MachineKind.Validate(); err != nil {
		return nil, runnerwire.Refuse("%v", err)
	}
	switch {
	case spec.RunID == uuid.Nil || spec.RunnerID != d.cfg.RunnerID:
		return nil, runnerwire.Refuse("an import must name this runner and a run id")
	case spec.MachineKind.Normalize() != "":
		return nil, runnerwire.Refuse("a moved run's sandbox is a container, never a %s", spec.MachineKind)
	}
	if err := runner.CheckImportSpec(spec); err != nil {
		return nil, runnerwire.Refuse("%v", err)
	}
	imp, ok := d.cfg.Substrate.(runner.StateImporter)
	if !ok {
		return nil, runnerwire.Refuse("this runner cannot take a moved run")
	}
	if err := d.cfg.BeforeCreate(ctx, spec); err != nil {
		return nil, err
	}
	stream, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindMoveImport, Target: spec.RunID.String(), Call: id})
	if err != nil {
		return nil, d.importFailed(ctx, spec, err)
	}
	go d.runImport(stream, imp, spec, a.Set)
	return runnerwire.StreamResult{Stream: stream.ID()}, nil
}

// runImport imports the files the org writes on stream, records the sandbox as a create does, and writes
// the end line.
func (d *Dispatcher) runImport(stream *runnerwire.Stream, imp runner.StateImporter, spec runner.SandboxSpec, set runner.CarriedSet) {
	ctx := stream.Context()
	// The label is what proves the sandbox this runner's own once its durable record is lost.
	filled := spec
	filled.Labels = maps.Clone(spec.Labels)
	if filled.Labels == nil {
		filled.Labels = map[string]string{}
	}
	filled.Labels[runner.LabelRunnerID] = d.cfg.RunnerID
	sandbox, manifest, err := imp.ImportSandbox(ctx, filled, set, stream)
	if err == nil {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err = d.cfg.Created(persist, spec, sandbox.Ref)
		cancel()
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			err = errors.Join(err, d.cfg.Substrate.KillSandbox(cleanup, sandbox.Ref))
			cancel()
		}
	}
	end := runnerwire.MoveImportEnd{}
	if err != nil {
		err = d.importFailed(ctx, spec, err)
		end.Error, end.Stage = runnerwire.ErrorFor(err), runnerwire.MoveImportStageCreate
		if errors.Is(err, runner.ErrMoveFiles) {
			end.Stage = runnerwire.MoveImportStageFiles
		}
	} else if err = d.finishMutation(ctx, runnerwire.MethodCreateSandbox, "", spec.RunID, uuid.Nil, nil); err != nil {
		end.Error, end.Stage = runnerwire.ErrorFor(err), runnerwire.MoveImportStageFiles
	} else {
		sandbox.Ref = placement.RunnerRefPrefix(d.cfg.RunnerID) + sandbox.Ref
		end.Sandbox, end.Manifest = &sandbox, manifest
	}
	b, merr := runnerwire.Marshal(end)
	if merr == nil {
		_, merr = stream.Write(append(b, '\n'))
	}
	if merr != nil {
		slog.Warn("runner: a move's import outcome was not sent", slog.String("run_id", spec.RunID.String()), slog.String("error", merr.Error()))
		_ = stream.Reset(runnerwire.ResetInternal)
		return
	}
	_ = stream.CloseWrite()
}

// importFailed contains a failed import's creation intent as a failed create's, and finishes it.
func (d *Dispatcher) importFailed(ctx context.Context, spec runner.SandboxSpec, cause error) error {
	contain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	cause = errors.Join(cause, d.cfg.CreateFailed(contain, spec))
	return d.finishMutation(ctx, runnerwire.MethodCreateSandbox, "", spec.RunID, uuid.Nil, cause)
}

// moveUnbound records the org's notice that a run was moved away from a sandbox of this runner.
func (d *Dispatcher) moveUnbound(ctx context.Context, args json.RawMessage) (any, error) {
	var a runner.MoveUnbound
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if d.cfg.MoveUnbound == nil {
		return nil, runnerwire.Refuse("this runner takes no move notices")
	}
	ref, err := d.local(a.Ref)
	if err != nil {
		return nil, err
	}
	return nil, d.cfg.MoveUnbound(ctx, a.RunID, ref)
}
