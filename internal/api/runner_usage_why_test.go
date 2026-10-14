// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// measuringRunner is a fakeRunner whose substrate measures what a test says.
type measuringRunner struct {
	*fakeRunner
	amounts types.RunnerAmounts
	err     error
}

func (m measuringRunner) Name() string { return "k8s" }
func (m measuringRunner) Measure(context.Context) (types.RunnerAmounts, error) {
	return m.amounts, m.err
}

// A substrate that could not make a read says which, and it reaches the runner's usage beside what it
// did measure; one that measured everything says nothing.
func TestRemoteUsageCarriesWhyAnAmountIsUnknown(t *testing.T) {
	s := &Server{cfg: Config{Runner: measuringRunner{fakeRunner: &fakeRunner{}, err: errors.New("no ResourceQuota in runs and k8s.readNodes is off")}}}
	rr := types.RemoteRunner{ID: "k8s"}
	if u := s.remoteUsage(context.Background(), rr, types.RunnerTally{}); u.ReportedWhy != "no ResourceQuota in runs and k8s.readNodes is off" || u.Reported != nil {
		t.Fatalf("usage = %+v, want the reason and nothing reported", u)
	}
	full := types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384, DiskMiB: 102400}
	s.cfg.Runner = measuringRunner{fakeRunner: &fakeRunner{}, amounts: full}
	if u := s.remoteUsage(context.Background(), rr, types.RunnerTally{}); u.ReportedWhy != "" || u.Reported == nil || *u.Reported != full {
		t.Fatalf("usage = %+v, want %+v and no reason", u, full)
	}
}
