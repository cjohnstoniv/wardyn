// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureRunnerLog(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// TestRelayWatchReprobesOnTheIntervalAndNamesADownRelay drives the watch with a fake ticker: each tick
// probes once, a relay that stops answering logs the named warning and reports unreachable, and the
// warning clears when it answers again.
func TestRelayWatchReprobesOnTheIntervalAndNamesADownRelay(t *testing.T) {
	logs := captureRunnerLog(t)
	var probes atomic.Int32
	var down atomic.Bool
	w := newRelayWatch(func(context.Context) error {
		probes.Add(1)
		if down.Load() {
			return errors.New("relay: nothing answers at 172.17.0.1:8443: connection refused")
		}
		return nil
	}, nil)
	ticks := make(chan time.Time)
	var interval time.Duration
	w.ticker = func(d time.Duration) (<-chan time.Time, func()) { interval = d; return ticks, func() {} }
	changed := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.run(ctx, func() { changed <- struct{}{} }); close(done) }()
	defer func() { cancel(); <-done }()

	tick := func() {
		t.Helper()
		before := probes.Load()
		ticks <- time.Now()
		ticks <- time.Now() // the second send waits until the first tick's check returned
		if got := probes.Load() - before; got < 1 {
			t.Fatalf("a tick probed %d times, want at least 1", got)
		}
	}
	tick()
	if interval != relayReprobeInterval {
		t.Fatalf("ticker interval %v, want relayReprobeInterval %v", interval, relayReprobeInterval)
	}
	if s := w.current(); s.State != types.RelayReachable || len(changed) != 0 {
		t.Fatalf("an answering relay: %+v, %d changes", s, len(changed))
	}
	down.Store(true)
	tick()
	if s := w.current(); s.State != types.RelayUnreachable || !strings.Contains(s.Reason, "connection refused") {
		t.Fatalf("a down relay reports %+v", s)
	}
	if !strings.Contains(logs.String(), relayUnreachableWarning) || !strings.Contains(logs.String(), "warning=relay_unreachable") {
		t.Fatalf("no named warning for a down relay: %s", logs.String())
	}
	if len(changed) != 1 {
		t.Fatalf("%d changes after the relay went down, want 1", len(changed))
	}
	down.Store(false)
	tick()
	if s := w.current(); s.State != types.RelayReachable || s.Reason != "" {
		t.Fatalf("a relay that answers again reports %+v", s)
	}
	if !strings.Contains(logs.String(), "warning_cleared=relay_unreachable") {
		t.Fatalf("the warning did not clear: %s", logs.String())
	}
}

// TestOfflineSummaryNeverHoldsAProxyPassword: a connect failure and a startup refusal whose text quotes
// the upstream proxy URL with its password keep neither the password nor the user, in memory or on
// disk; the startup refusal survives a restart and is cleared once the organisation acknowledged it.
func TestOfflineSummaryNeverHoldsAProxyPassword(t *testing.T) {
	const proxyURL = "http://alice:hunter2pw@proxy.corp.example:3128"
	dir := t.TempDir()
	l := newOfflineLog(dir, urlSecrets(proxyURL))
	l.connectFailed(fmt.Errorf("proxyconnect tcp: dial %s: refused; password hunter2pw", proxyURL))
	s := l.pending()
	if s == nil || strings.Contains(s.Reason, "hunter2pw") || strings.Contains(s.Reason, "alice") {
		t.Fatalf("connect summary %+v", s)
	}
	l.startFailed(atStage(types.RunnerStageEngine, fmt.Errorf("engine via %s: no socket", proxyURL)))
	b, err := os.ReadFile(filepath.Join(dir, startupFailureFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("hunter2pw")) || bytes.Contains(b, []byte("alice")) {
		t.Fatalf("startup failure file holds the credential: %s", b)
	}
	again := newOfflineLog(dir, nil)
	got := again.pending()
	if got == nil || got.Stage != types.RunnerStageEngine || !got.FirstFailureAt.Equal(s.FirstFailureAt) {
		t.Fatalf("after a restart the summary is %+v, want the engine refusal since %v", got, s.FirstFailureAt)
	}
	var caps runnerwire.Caps
	r := connectReport{log: again}
	if !r.fill(t.Context(), &caps, false) || caps.OfflineSummary == nil {
		t.Fatal("caps carry no offline summary")
	}
	r.sent(caps)
	if again.pending() != nil {
		t.Fatal("an acknowledged summary is sent again")
	}
	if _, err := os.Stat(filepath.Join(dir, startupFailureFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the startup failure file outlives its acknowledgement: %v", err)
	}
}

// TestRunnerStagesAreDoctorStageNames ties types.RunnerStages to the stage names `wardyn-runnerd doctor`
// emits: every stage an offline summary may name is the first argument of a doctor OKStage, FailStage
// or SkipStage call in internal/doctor or in this command's doctor.
func TestRunnerStagesAreDoctorStageNames(t *testing.T) {
	files, err := filepath.Glob("../../internal/doctor/*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "doctor.go")
	var names []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			fn := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				fn = f.Name
			case *ast.SelectorExpr:
				fn = f.Sel.Name
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING && slices.Contains([]string{"OKStage", "FailStage", "SkipStage"}, fn) {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					names = append(names, s)
				}
			}
			return true
		})
	}
	for _, s := range types.RunnerStages {
		if !slices.Contains(names, s) {
			t.Errorf("types.RunnerStages names %q, which no doctor stage emits (doctor emits %v)", s, names)
		}
	}
}
