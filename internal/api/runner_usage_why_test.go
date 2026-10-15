// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
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

// A substrate that could not make a read says which, and an operator's usage carries it beside what
// was measured; one that measured everything says nothing. A member's usage never carries it: the reason
// can hold the Kubernetes API's own error text.
func TestRemoteUsageCarriesWhyAnAmountIsUnknown(t *testing.T) {
	const why = "no ResourceQuota in runs and k8s.readNodes is off"
	s := &Server{cfg: Config{Runner: measuringRunner{fakeRunner: &fakeRunner{}, err: errors.New(why)}}}
	rr := types.RemoteRunner{ID: "k8s"}
	operator := operatorCtx("admin", "admin@corp.example", oidc.RoleAdmin)
	member := operatorCtx("alice", "alice@corp.example", oidc.RoleUser)
	if u := s.remoteUsage(operator, rr, types.RunnerTally{}); u.ReportedWhy != why || u.Reported != nil {
		t.Fatalf("operator usage = %+v, want the reason and nothing reported", u)
	}
	if u := s.remoteUsage(member, rr, types.RunnerTally{}); u.ReportedWhy != "" {
		t.Fatalf("member usage = %+v, want no reason", u)
	}
	full := types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384, DiskMiB: 102400}
	s.cfg.Runner = measuringRunner{fakeRunner: &fakeRunner{}, amounts: full}
	if u := s.remoteUsage(operator, rr, types.RunnerTally{}); u.ReportedWhy != "" || u.Reported == nil || *u.Reported != full {
		t.Fatalf("usage = %+v, want %+v and no reason", u, full)
	}
}

// remoteRunnersFake serves a fixed list of remote-hosted runner rows beside the registration fake.
type remoteRunnersFake struct {
	*runnerRegistrationFake
	store.RemoteRunnerStore
	rows []store.RemoteRunnerRow
}

func (f remoteRunnersFake) ListRemoteRunners(context.Context) ([]store.RemoteRunnerRow, error) {
	return f.rows, nil
}

// The runner_limits row counts a remote-hosted runner with no limits only while this deployment runs it.
func TestRunnerLimitsRowCountsConfiguredRemoteRunners(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	srv.cfg.Runner = measuringRunner{fakeRunner: &fakeRunner{}} // its executor is "k8s"
	row := func(id string, l *types.RunnerLimits) store.RemoteRunnerRow {
		return store.RemoteRunnerRow{Runner: types.RemoteRunner{ID: id, Limits: l}}
	}
	srv.cfg.Store = remoteRunnersFake{runnerRegistrationFake: f, rows: []store.RemoteRunnerRow{
		row("k8s", nil), row("gone", nil), row("k8s-other", &types.RunnerLimits{RunsAtOnce: 2}),
	}}
	chk, ok := srv.runnerLimitsRow(context.Background())
	if !ok || chk.Status != "warn" || chk.Detail[:len("1 runner has")] != "1 runner has" {
		t.Fatalf("row = %+v, %v; want a warn counting only the configured remote runner with no limits", chk, ok)
	}
}

// A read the substrate could not make is not a warning each time usage is built: usage is built on every
// runner read, and the boot path is where the operator is told.
func TestMeasureRemoteDoesNotWarnOnEveryRead(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	s := &Server{cfg: Config{Runner: measuringRunner{fakeRunner: &fakeRunner{}, err: errors.New("no ResourceQuota")}}}
	if _, why := s.measureRemote(context.Background(), "k8s"); why != "no ResourceQuota" {
		t.Fatalf("why = %q", why)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("measureRemote warned on a read:\n%s", buf.String())
	}
}
