// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A Kubernetes runner sends no Caps.MoveImportPresent, which wardyn-runnerd derives from the Driver not being
// a runner.MoveImportPresenter: its kept or missing pod reads absent to the runner's observer, so a run
// moved to it would read gone before it starts.
func TestDriverIsNoMoveImportPresenter(t *testing.T) {
	if _, ok := any(&Driver{}).(runner.MoveImportPresenter); ok {
		t.Fatal("the k8s Driver is a runner.MoveImportPresenter; runnerd would advertise a destination whose imported pod reads absent")
	}
}
