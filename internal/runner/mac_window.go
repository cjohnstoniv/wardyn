// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "errors"

// Why a window ended before its owner closed it, as MacWindow.Reason says.
const (
	MacWindowEndPerson       = "closed_by_person" // the person closed the window
	MacWindowEndHostLocked   = "host_locked"      // the Mac's screen locked or its console session changed
	MacWindowEndRunEnded     = "run_ended"        // the VM stopped
	MacWindowEndHelperFailed = "helper_failed"    // the helper could not keep the window
	MacWindowEndHelperLost   = "helper_lost"      // the helper's connection ended without saying why
)

// Window refusals a MacWindower can answer; a MacWindowRefusal carries the helper's own, and any other
// error is the runner's own failure.
var (
	// ErrMacWindowUnsupported: the sandbox is no macOS VM, or this runner cannot reach its helper.
	ErrMacWindowUnsupported = errors.New("runner: this sandbox has no window to open")
	// ErrMacWindowOpen: the run's window is already open.
	ErrMacWindowOpen = errors.New("runner: this run's window is already open")
	// ErrMacWindowHelperOld: the run's helper does not know the window request, so it started before
	// the runner was upgraded.
	ErrMacWindowHelperOld = errors.New("runner: this run's macOS VM helper cannot open a window")
)

// MacWindowRefusal is a refusal the run's helper worded: Code is its window_* code and Sentence what the
// person reads.
type MacWindowRefusal struct{ Code, Sentence string }

func (e *MacWindowRefusal) Error() string { return e.Sentence }
