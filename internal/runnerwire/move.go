// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// MoveExportArgs is move_export's input: the stopped, kept sandbox Ref names and what to carry out of it.
// The runner answers MoveExportResult and opens a KindMoveExport stream carrying the files.
type MoveExportArgs struct {
	Ref string            `json:"ref"`
	Set runner.CarriedSet `json:"set"`
}

// MoveExportResult is move_export's answer: the stream the runner opened, and whether the run's spool on
// the runner was delivered when the export started (runner.StateExport).
type MoveExportResult struct {
	Stream       uint32 `json:"stream"`
	SpoolDrained bool   `json:"spool_drained,omitempty"`
}

// MoveImportArgs is move_import's input: the destination sandbox's spec, which carries no proxy config
// (runner.CheckImportSpec), and what the carried files hold. The runner answers StreamResult with a
// KindMoveImport stream.
type MoveImportArgs struct {
	Spec runner.SandboxSpec `json:"spec"`
	Set  runner.CarriedSet  `json:"set"`
}

// MoveImportEnd is the one line the runner writes on a move_import stream once the import has ended:
// the sandbox and the manifest its own reader computed, or the error and the half that failed
// (MoveImportStageCreate or MoveImportStageFiles).
type MoveImportEnd struct {
	Sandbox  *runner.Sandbox        `json:"sandbox,omitempty"`
	Manifest runner.CarriedManifest `json:"manifest"`
	Stage    string                 `json:"stage,omitempty"`
	Error    *Error                 `json:"error,omitempty"`
}

// MoveImportEnd.Stage values.
const (
	MoveImportStageCreate = "create"
	MoveImportStageFiles  = "files"
)

// MoveImportEndMaxBytes bounds the line the org reads.
const MoveImportEndMaxBytes = 64 << 10
