// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// TestEveryLeaderSweepHasAFamily: every leaderGo call in this package names its sweep with a string literal that
// sweepFamilies maps to a family in db.SweeperFamilies, and every entry of the map is a sweep that is started.
// Catches: a new leader sweep with no family, which leaderGo refuses to run.
func TestEveryLeaderSweepHasAFamily(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	started := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "leaderGo" {
				return true
			}
			pos := fset.Position(call.Pos())
			var lit *ast.BasicLit
			if len(call.Args) == 4 {
				lit, _ = call.Args[2].(*ast.BasicLit)
			}
			if lit == nil || lit.Kind != token.STRING {
				t.Errorf("%s: leaderGo's sweep name is not a string literal", pos)
				return true
			}
			sweep, _ := strconv.Unquote(lit.Value)
			started[sweep] = true
			family, ok := sweepFamily(sweep)
			if !ok {
				t.Errorf("%s: leader sweep %q has no sweep family in sweepFamilies", pos, sweep)
			} else if !slices.Contains(db.SweeperFamilies, family) {
				t.Errorf("%s: leader sweep %q maps to %q, not a family in db.SweeperFamilies", pos, sweep, family)
			}
			return true
		})
	}
	if len(started) == 0 {
		t.Fatal("found no leaderGo call")
	}
	for sweep := range sweepFamilies {
		if !started[sweep] {
			t.Errorf("sweepFamilies names %q, which no leaderGo call starts", sweep)
		}
	}
}

// fakeTicks is a sweep health store that answers a fixed record.
type fakeTicks map[string]sweephealth.Tick

func (fakeTicks) RecordTick(context.Context, string, string, bool, time.Time) error { return nil }

func (f fakeTicks) Ticks(context.Context) (map[string]sweephealth.Tick, error) { return f, nil }

// fakeFamily is a sweep family that counts its resigns.
type fakeFamily struct {
	acquired time.Time
	held     bool
	resigned int
}

func (f *fakeFamily) Held() (time.Time, bool) { return f.acquired, f.held }

func (f *fakeFamily) Join() (context.Context, int64, func(), bool) {
	return context.Background(), 7, func() {}, f.held
}

func (f *fakeFamily) Resign() { f.resigned++ }

// watchdogTracker is a tracker over ticks with every watched sweep registered at a one-minute interval, so a sweep
// is stalled three minutes after the later of its last success and the family's acquisition.
func watchdogTracker(now time.Time, ticks fakeTicks) *sweephealth.Tracker {
	tr := sweephealth.New(ticks, "replica-a", func() time.Time { return now })
	for _, names := range watchedSweeps {
		for _, name := range names {
			tr.Register(sweephealth.Sweep{Name: name, Interval: time.Minute})
		}
	}
	return tr
}

// TestWatchdogResignsOnlyStalledFamily: of the families this replica leads, only the one whose every watched sweep
// has stalled is given up; a family with a fresh sweep, and the audit family, which has no watched sweep, keep
// running.
func TestWatchdogResignsOnlyStalledFamily(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	stale, fresh := now.Add(-10*time.Minute), now.Add(-30*time.Second)
	ticks := fakeTicks{
		sweephealth.RunQueue: {SucceededAt: stale}, sweephealth.RunOutput: {SucceededAt: stale},
		sweephealth.RunWatcher:       {SucceededAt: stale},
		sweephealth.CredentialExpiry: {SucceededAt: stale}, sweephealth.AWSSSORenewal: {SucceededAt: fresh},
		sweephealth.ApprovalExpiry: {SucceededAt: fresh}, sweephealth.RecordingRetention: {SucceededAt: fresh},
	}
	long := now.Add(-time.Hour)
	fams := map[string]*fakeFamily{}
	in := map[string]watchedFamily{}
	for _, name := range db.SweeperFamilies {
		fams[name] = &fakeFamily{acquired: long, held: true}
		in[name] = fams[name]
	}
	sweeperWatchdogPass(context.Background(), in, watchdogTracker(now, ticks), now)
	for name, f := range fams {
		want := 0
		if name == db.SweeperFamilyRuns {
			want = 1
		}
		if f.resigned != want {
			t.Errorf("family %q resigned %d times; want %d", name, f.resigned, want)
		}
	}
}

// TestWatchdogJudgesSucceededAtNotAttempts: staleness is judged on the last success and the family's acquisition,
// never the last attempt, which followers write too; and a family with no watched sweep is never given up.
func TestWatchdogJudgesSucceededAtNotAttempts(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	stale, fresh, long := now.Add(-10*time.Minute), now.Add(-30*time.Second), now.Add(-time.Hour)
	all := func(tk sweephealth.Tick) fakeTicks {
		out := fakeTicks{}
		for _, names := range watchedSweeps {
			for _, name := range names {
				out[name] = tk
			}
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		family   string
		ticks    fakeTicks
		acquired time.Time
		want     int
	}{
		{"fresh attempt, stale success", db.SweeperFamilyCredentials, all(sweephealth.Tick{AttemptedAt: now, SucceededAt: stale}), long, 1},
		{"fresh success", db.SweeperFamilyCredentials, all(sweephealth.Tick{AttemptedAt: now, SucceededAt: fresh}), long, 0},
		{"acquired 10 s ago", db.SweeperFamilyCredentials, all(sweephealth.Tick{SucceededAt: stale}), now.Add(-10 * time.Second), 0},
		{"no watched sweep", db.SweeperFamilyAudit, all(sweephealth.Tick{SucceededAt: stale}), long, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFamily{acquired: tc.acquired, held: true}
			sweeperWatchdogPass(context.Background(), map[string]watchedFamily{tc.family: f}, watchdogTracker(now, tc.ticks), now)
			if f.resigned != tc.want {
				t.Fatalf("resigned %d times; want %d", f.resigned, tc.want)
			}
		})
	}
}
