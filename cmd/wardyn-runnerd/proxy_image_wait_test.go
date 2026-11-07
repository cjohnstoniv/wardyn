// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func imageMissing() error {
	return atStage(types.RunnerStageProxyImage, errors.New("relay: the proxy image ghcr.io/x/proxy:1 is not on this engine and could not be pulled"))
}

// waitRig records what waitForProxyImage sleeps and reports.
type waitRig struct {
	delays  []time.Duration
	reports []time.Time
	errs    []error
}

func (r *waitRig) sleep(_ context.Context, d time.Duration) error {
	r.delays = append(r.delays, d)
	return nil
}

func (r *waitRig) report(err error, next time.Time) {
	r.errs = append(r.errs, err)
	r.reports = append(r.reports, next)
}

func withinJitter(t *testing.T, got, want time.Duration) {
	t.Helper()
	if lo, hi := want-want/10, want+want/10; got < lo || got > hi {
		t.Errorf("delay %v, want %v within 10%% (%v..%v)", got, want, lo, hi)
	}
}

func TestWaitForProxyImageRetriesWithBackoffUntilPresent(t *testing.T) {
	r := &waitRig{}
	calls := 0
	attempt := func(context.Context) error {
		calls++
		if calls <= 3 {
			return imageMissing()
		}
		return nil
	}
	before := time.Now()
	if err := waitForProxyImage(context.Background(), "ghcr.io/x/proxy:1", attempt, r.report, r.sleep); err != nil {
		t.Fatalf("waitForProxyImage = %v, want nil once the image is present", err)
	}
	if calls != 4 || len(r.delays) != 3 {
		t.Fatalf("attempts %d, sleeps %d, want 4 and 3", calls, len(r.delays))
	}
	for i, want := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute} {
		withinJitter(t, r.delays[i], want)
		if next := r.reports[i]; next.Before(before.Add(r.delays[i])) || next.After(time.Now().Add(r.delays[i])) {
			t.Errorf("report %d next try %v, want now plus the delay %v", i, next, r.delays[i])
		}
	}
	// Three failures are reported with their error; the success clears the report with a nil error.
	if len(r.errs) != 4 || r.errs[0] == nil || r.errs[3] != nil || !r.reports[3].IsZero() {
		t.Errorf("reports %v %v, want three failures then a clearing nil report", r.errs, r.reports)
	}
}

func TestWaitForProxyImageCapsTheDelayAtFiveMinutes(t *testing.T) {
	r := &waitRig{}
	calls := 0
	attempt := func(context.Context) error {
		if calls++; calls <= 7 {
			return imageMissing()
		}
		return nil
	}
	if err := waitForProxyImage(context.Background(), "img", attempt, r.report, r.sleep); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if len(r.delays) != len(want) {
		t.Fatalf("sleeps %v, want %d", r.delays, len(want))
	}
	for i := range want {
		withinJitter(t, r.delays[i], want[i])
	}
}

func TestWaitForProxyImageReturnsOtherStagesAtOnce(t *testing.T) {
	for name, fail := range map[string]error{
		"relay":    atStage(types.RunnerStageRelay, errors.New("no relay")),
		"engine":   atStage(types.RunnerStageEngine, errors.New("no engine")),
		"no stage": errors.New("bad flag"),
	} {
		t.Run(name, func(t *testing.T) {
			r := &waitRig{}
			calls := 0
			err := waitForProxyImage(context.Background(), "img", func(context.Context) error { calls++; return fail }, r.report, r.sleep)
			if err != fail || calls != 1 || len(r.delays) != 0 || len(r.reports) != 0 {
				t.Errorf("err %v, attempts %d, sleeps %v, reports %v, want the error as is after one attempt and no sleep", err, calls, r.delays, r.reports)
			}
		})
	}
}

func TestWaitForProxyImageEndsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	reports := 0
	err := waitForProxyImage(ctx, "img",
		func(context.Context) error { calls++; return imageMissing() },
		func(error, time.Time) { reports++ },
		func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled so the service exits 0", err)
	}
	var se *stageError
	if !errors.As(err, &se) || se.stage != types.RunnerStageProxyImage {
		t.Errorf("err = %v, want it to keep the proxy-image stage", err)
	}
	if calls != 1 || reports != 1 {
		t.Errorf("attempts %d, reports %d, want one of each: a stop is not retried", calls, reports)
	}
	// A real sleep returns at once on a cancelled context.
	start := time.Now()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Minute {
		t.Errorf("sleepCtx = %v after %v, want context.Canceled at once", err, time.Since(start))
	}
}

func TestStatusShowsTheNextImageTry(t *testing.T) {
	prev := statusNow
	statusNow = func() time.Time { return statusAt }
	t.Cleanup(func() { statusNow = prev })
	r := newRig(t)
	next := statusAt.Add(30 * time.Second)
	st := offlineAt(types.RunnerStageProxyImage, "relay: the proxy image x is not on this engine")
	st.State, st.NextRetryAt = "starting", &next
	r.writeStatus(t, "", st)
	s := stage(t, r.run(doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}), "status")
	if want := "waiting for the proxy image; next try at 2026-10-21T08:30:30Z"; !strings.Contains(s.Detail, want) {
		t.Errorf("status detail %q, want it to contain %q", s.Detail, want)
	}
	// A runner that has stopped is not waiting, whatever a killed process left in the file.
	st.State = "stopped"
	r.writeStatus(t, "", st)
	if s := stage(t, r.run(doctorOptions{}), "status"); strings.Contains(s.Detail, "next try") {
		t.Errorf("stopped runner status %q, want no next try", s.Detail)
	}
}

// A next try that is already in the past is not shown: the file is a runner's leftover.
func TestStatusHidesANextImageTryInThePast(t *testing.T) {
	prev := statusNow
	statusNow = func() time.Time { return statusAt.Add(time.Minute) }
	t.Cleanup(func() { statusNow = prev })
	r := newRig(t)
	next := statusAt.Add(30 * time.Second)
	st := offlineAt(types.RunnerStageProxyImage, "relay: the proxy image x is not on this engine")
	st.State, st.NextRetryAt = "starting", &next
	r.writeStatus(t, "", st)
	if s := stage(t, r.run(doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}), "status"); strings.Contains(s.Detail, "next try") {
		t.Errorf("status detail %q, want no next try once it is in the past", s.Detail)
	}
}

func TestWaitingKeepsTheStartupFailureForTheOrganisation(t *testing.T) {
	dir := t.TempDir()
	log := newOfflineLog(dir, nil)
	status := newStatusWriter(dir)
	log.reportTo(status)
	next := time.Now().Add(15 * time.Second).UTC()
	log.waiting(imageMissing(), next)

	var sum types.RunnerOfflineSummary
	b, err := os.ReadFile(filepath.Join(dir, startupFailureFile))
	if err != nil {
		t.Fatalf("startup failure not kept while waiting: %v", err)
	}
	if err := json.Unmarshal(b, &sum); err != nil || sum.Stage != types.RunnerStageProxyImage || !strings.Contains(sum.Reason, "proxy image") {
		t.Errorf("startup-failure.json = %s (%v), want stage %s with the reason", b, err, types.RunnerStageProxyImage)
	}
	if p := log.pending(); p == nil || p.Stage != types.RunnerStageProxyImage {
		t.Errorf("pending summary %+v, want the proxy-image failure for the next connect", p)
	}
	st, err := loadStatus(statusPath(dir))
	if err != nil || st.NextRetryAt == nil || !st.NextRetryAt.Equal(next) || st.Stage != types.RunnerStageProxyImage {
		t.Fatalf("status = %+v (%v), want stage proxy-image and next_retry_at %v", st, err, next)
	}

	// The image arrives: the next try is gone and the stage no longer shows, but the organisation is
	// still told why the runner was late.
	log.waiting(nil, time.Time{})
	st, _ = loadStatus(statusPath(dir))
	if st.NextRetryAt != nil || st.Stage != "" {
		t.Errorf("status after the image arrived = %+v, want no next try and no stage", st)
	}
	if _, err := os.Stat(filepath.Join(dir, startupFailureFile)); err != nil {
		t.Errorf("startup-failure.json gone after the image arrived: %v", err)
	}
	// stopped never leaves a next try behind.
	log.waiting(imageMissing(), next)
	status.stopped()
	if st, _ = loadStatus(statusPath(dir)); st.NextRetryAt != nil {
		t.Errorf("status after stop = %+v, want no next try", st)
	}
}

// The start's wait as production wires it (the offline log's waiting as the report): while the image is
// missing, status.json shows the proxy-image stage, the reason and the next try, and a stop during the
// wait returns at once. The sleep is faked; the whole wait is bounded in seconds.
func TestWaitingStatusCarriesTheReasonAndTheNextTry(t *testing.T) {
	dir := t.TempDir()
	log := newOfflineLog(dir, nil)
	log.reportTo(newStatusWriter(dir))
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	before := time.Now()
	var seen runnerStatus
	sleep := func(ctx context.Context, d time.Duration) error {
		st, err := loadStatus(statusPath(dir))
		if err != nil {
			t.Errorf("status while waiting: %v", err)
		}
		seen = st
		stop() // a stop arrives during the wait
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		done <- waitForProxyImage(ctx, "ghcr.io/x/proxy:1", func(context.Context) error { return imageMissing() }, log.waiting, sleep)
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end within 5 seconds of a stop")
	}
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "stopped while waiting for the proxy image") {
		t.Errorf("waitForProxyImage = %v, want the image failure joined with the stop", err)
	}
	if seen.Stage != types.RunnerStageProxyImage || !strings.Contains(seen.LastError, "proxy image ghcr.io/x/proxy:1 is not on this engine") {
		t.Errorf("status while waiting = stage %q, last error %q; want the proxy-image stage and its reason", seen.Stage, seen.LastError)
	}
	first := proxyImageDelays[0]
	if seen.NextRetryAt == nil || seen.NextRetryAt.Before(before.Add(first-first/10)) || seen.NextRetryAt.After(time.Now().Add(first+first/10)) {
		t.Errorf("status next_retry_at = %v, want about %v after the first try", seen.NextRetryAt, first)
	}
}
