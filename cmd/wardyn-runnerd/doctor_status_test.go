// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var statusAt = time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC)

// writeStatus puts a status.json where the rig's service keeps it (dir "" is the default state directory).
func (r *doctorRig) writeStatus(t *testing.T, dir string, st runnerStatus) {
	t.Helper()
	if dir == "" {
		dir = filepath.Join(r.home, ".config", "wardyn", "runner")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, statusFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func offlineAt(stage, reason string) runnerStatus {
	return runnerStatus{State: "offline", Since: statusAt, LastError: reason, LastErrorAt: &statusAt, Stage: stage}
}

// TestStatusStageNamesTheSettingOfTheStageItStoppedAt fakes a status.json per stage the service records.
func TestStatusStageNamesTheSettingOfTheStageItStoppedAt(t *testing.T) {
	for _, tc := range []struct {
		stage, setting string
	}{
		{types.RunnerStageDNS, orgSetting},
		{types.RunnerStageProxy, "HTTPS_PROXY"},
		{types.RunnerStageTLS, "WARDYN_TRUSTED_CA_FILE"},
		{types.RunnerStageHealthz, orgSetting},
		{types.RunnerStageEngine, "DOCKER_HOST"},
		{types.RunnerStageProxyImage, "WARDYN_PROXY_IMAGE"},
		{types.RunnerStageRelay, "WARDYN_RELAY_LISTEN"},
		{"", orgSetting},
	} {
		t.Run("stage "+tc.stage, func(t *testing.T) {
			r := newRig(t)
			r.writeStatus(t, "", offlineAt(tc.stage, "the organisation did not answer"))
			s := stage(t, r.run(doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}), "status")
			if s.Status != doctor.StatusFail || s.Setting != tc.setting || !strings.Contains(s.Fix, "set "+tc.setting+" to") ||
				!strings.Contains(s.Detail, "2026-10-21T08:30:00Z") || !strings.Contains(s.Detail, "the organisation did not answer") {
				t.Errorf("status = %s setting %q detail %q fix %q, want FAIL naming %s with the time and the error", s.Status, s.Setting, s.Detail, s.Fix, tc.setting)
			}
		})
	}
}

func TestStatusStageForARefusedIdentityAsksToRegisterAgain(t *testing.T) {
	r := newRig(t)
	r.writeStatus(t, "", offlineAt(types.RunnerStageIdentity, "the organisation refused this runner's proof"))
	s := stage(t, r.run(doctorOptions{}), "status")
	if s.Status != doctor.StatusFail || !strings.Contains(s.Fix, "register this runner again") {
		t.Errorf("status = %s %q %q, want FAIL that says to register again", s.Status, s.Detail, s.Fix)
	}
}

func TestStatusStageWithoutAFileSaysTheServiceHasNotRunAndDialsNothingAsTheRunner(t *testing.T) {
	r := newRig(t)
	rep := r.run(doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"})
	if s := stage(t, rep, "status"); s.Status != doctor.StatusSkip || !strings.Contains(s.Detail, "has not run yet") {
		t.Errorf("status = %s %q, want a skip that says the service has not run yet", s.Status, s.Detail)
	}
	for _, s := range rep.Stages {
		if s.Name == "identity" {
			t.Error("a runner's doctor must not sign in as the runner")
		}
	}
	if _, err := os.Stat(filepath.Join(r.home, ".config", "wardyn", "runner", statusFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("doctor wrote a status file: %v", err)
	}
}

func TestStatusStageReadsTheStateDirOfTheFlag(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.writeStatus(t, dir, offlineAt(types.RunnerStageTLS, "x509: unknown authority"))
	if s := stage(t, r.run(doctorOptions{stateDir: dir}), "status"); s.Status != doctor.StatusFail || !strings.Contains(s.Detail, "unknown authority") {
		t.Errorf("status = %s %q, want the file of --state-dir", s.Status, s.Detail)
	}
}

func TestStatusStageConnectedReportsAnOldErrorWithoutFailing(t *testing.T) {
	r := newRig(t)
	st := offlineAt("", "link down")
	st.State = "connected"
	r.writeStatus(t, "", st)
	if s := stage(t, r.run(doctorOptions{}), "status"); s.Status != doctor.StatusOK || !strings.Contains(s.Detail, "connected since") || !strings.Contains(s.Detail, "link down") {
		t.Errorf("status = %s %q, want ok that carries the last error and its time", s.Status, s.Detail)
	}
}

// While the docker driver serves any peer because the organisation's proxy image refused allowed_peers
// (docker.Config.PeersOnlyFallback), status.json names that image and doctor's status stage says peers-only
// is not in effect, without failing; once a proxy takes allowed_peers again the warning is gone.
func TestStatusStageSaysPeersOnlyIsNotInEffectWhileTheOrganisationIsOlder(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(r.home, ".config", "wardyn", "runner")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l := newOfflineLog(dir, nil)
	l.reportTo(newStatusWriter(dir))
	const image = releaseProxyRepo + ":0.9.8"
	l.peersOnlyChanged(image)
	s := stage(t, r.run(doctorOptions{}), "status")
	if s.Status != doctor.StatusOK || !strings.Contains(s.Detail, "peers-only not in effect: organisation older") || !strings.Contains(s.Detail, image) {
		t.Errorf("status = %s %q, want ok that says peers-only is not in effect and names %s", s.Status, s.Detail, image)
	}
	l.peersOnlyChanged("")
	if s := stage(t, r.run(doctorOptions{}), "status"); strings.Contains(s.Detail, "peers-only") {
		t.Errorf("status = %s %q, want no peers-only warning once a proxy takes allowed_peers", s.Status, s.Detail)
	}
}

// TestStatusStageStoppedOrStartingAfterAnOldDropIsNotAFailure: linkChanged keeps the last error across a
// reconnect and a clean shutdown records no stage, so a runner that dropped once and later stopped cleanly
// (or is starting again) must not be told to change its organisation address.
func TestStatusStageStoppedOrStartingAfterAnOldDropIsNotAFailure(t *testing.T) {
	for _, state := range []string{"stopped", "starting"} {
		t.Run(state, func(t *testing.T) {
			r := newRig(t)
			st := offlineAt("", "link down")
			st.State = state
			r.writeStatus(t, "", st)
			s := stage(t, r.run(doctorOptions{}), "status")
			if s.Status == doctor.StatusFail || s.Setting != "" || !strings.Contains(s.Detail, "link down") {
				t.Errorf("status = %s setting %q %q, want a non-failure that still carries the old error", s.Status, s.Setting, s.Detail)
			}
		})
	}
}

func TestStatusStageCorruptFileNamesTheStateDir(t *testing.T) {
	r := newRig(t)
	dir := filepath.Join(r.home, ".config", "wardyn", "runner")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, statusFile), []byte("{not json"), 0o600)
	if s := stage(t, r.run(doctorOptions{}), "status"); s.Status != doctor.StatusFail || s.Setting != "WARDYN_RUNNER_STATE_DIR" {
		t.Errorf("status = %s setting %q %q, want FAIL naming WARDYN_RUNNER_STATE_DIR", s.Status, s.Setting, s.Detail)
	}
}

func TestStatusStageNeverPrintsACredentialFromTheFile(t *testing.T) {
	r := newRig(t)
	r.writeStatus(t, "", offlineAt(types.RunnerStageProxy, "proxyconnect via https://deploy:hunter2-secret@proxy.example.com:3128 refused"))
	rep := r.run(doctorOptions{})
	var text, js strings.Builder
	_ = rep.WriteText(&text)
	_ = rep.WriteJSON(&js)
	for _, out := range []string{text.String(), js.String()} {
		if strings.Contains(out, "hunter2-secret") {
			t.Errorf("the output carries a credential:\n%s", out)
		}
	}
}

func TestServiceStage(t *testing.T) {
	t.Run("not installed is a skip, not a failure", func(t *testing.T) {
		r := newRig(t)
		if s := stage(t, r.run(doctorOptions{}), "service"); s.Status != doctor.StatusSkip || !strings.Contains(s.Detail, "no service is installed") {
			t.Errorf("service = %s %q", s.Status, s.Detail)
		}
	})
	t.Run("active", func(t *testing.T) {
		r := newRig(t)
		r.installUnit(t)
		if s := stage(t, r.run(doctorOptions{}), "service"); s.Status != doctor.StatusOK || !strings.Contains(s.Detail, "wardyn-runnerd.service is active") {
			t.Errorf("service = %s %q", s.Status, s.Detail)
		}
	})
	t.Run("installed but not running", func(t *testing.T) {
		r := newRig(t)
		r.installUnit(t)
		r.systemd, r.systemdErr = "inactive", errors.New("exit status 3")
		s := stage(t, r.run(doctorOptions{}), "service")
		if s.Status != doctor.StatusFail || !strings.Contains(s.Detail, "reports it inactive") || !strings.Contains(s.Fix, "systemctl --user start wardyn-runnerd.service") {
			t.Errorf("service = %s %q %q, want FAIL with the command that starts it", s.Status, s.Detail, s.Fix)
		}
	})
	t.Run("systemd gives no answer", func(t *testing.T) {
		r := newRig(t)
		r.installUnit(t)
		r.systemd, r.systemdErr = "", errors.New("systemctl: not found")
		if s := stage(t, r.run(doctorOptions{}), "service"); s.Status != doctor.StatusSkip {
			t.Errorf("service = %s %q, want skip: doctor cannot tell", s.Status, s.Detail)
		}
	})
}

// A start refused with no stage (an invalid --local-attach value) is a failure at rest, not a clean stop
// after an old connection error: doctor fails it in text and JSON, finishDoctor returns the error that makes
// the command exit non-zero, and it stays failed until a start connects, through a retry, a failed connect
// and a hard kill.
func TestStatusStageStartRefusedWithoutAStageFailsUntilAStartSucceeds(t *testing.T) {
	r := newRig(t)
	state := filepath.Join(r.home, ".config", "wardyn", "runner")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	_, refusal := (serveOptions{localAttach: "maybe"}).localAttachEnabled()
	if refusal == nil {
		t.Fatal("--local-attach=maybe was accepted")
	}
	now := statusAt
	tick := func() *time.Time { now = now.Add(time.Minute); return &now }
	// start is serve's paired initialisation: the offline log (which reads the startup summary an earlier
	// start kept) reports to a new status writer (which reads the previous status.json).
	start := func() (*statusWriter, *offlineLog) { return stageFixture(t, state, tick()) }
	status := func() runnerStatus {
		t.Helper()
		st, err := loadStatus(statusPath(state))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	fails := func(what, want string) doctor.Report {
		t.Helper()
		rep := r.run(doctorOptions{})
		s := stage(t, rep, "status")
		if s.Status != doctor.StatusFail || !strings.Contains(s.Detail, want) {
			t.Fatalf("%s: status = %s %q, want FAIL naming %q", what, s.Status, s.Detail, want)
		}
		return rep
	}

	// A: a staged refusal; its summary stays in the startup file.
	w, l := start()
	l.startFailed(atStage(types.RunnerStageEngine, errors.New("engine: no socket")))
	w.stopped()
	fails("staged refusal A", "stage engine")

	// B: a newer refusal with no stage. The pending A is replayed first, then B replaces it.
	w, l = start()
	l.startFailed(refusal)
	w.stopped()
	if st := status(); st.StartRefusal == nil || st.Stage != "" || st.State != "stopped" {
		t.Fatalf("status at rest after B = %+v, want stopped, no stage, a start refusal", st)
	}
	rep := fails("B at rest", "local attach must be on or off")
	for _, asJSON := range []bool{false, true} {
		var out strings.Builder
		if err := finishDoctor(&out, rep, asJSON); err == nil {
			t.Errorf("json=%v: finishDoctor returned nil, want the error that exits non-zero", asJSON)
		}
		want := "FAIL  status"
		if asJSON {
			want = `"status": "FAIL"`
		}
		if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "local attach must be on or off") {
			t.Errorf("json=%v: output lacks the failed status stage (%q):\n%s", asJSON, want, out.String())
		}
	}

	// C: a retry that has not connected, and one interrupted before it connects. The summary of A is still
	// on disk but older than B: it must not be replayed over it.
	w, l = start()
	_ = l
	if st := status(); st.State != "starting" || st.StartRefusal == nil || st.Stage != "" {
		t.Fatalf("a new writer's file = %+v, want starting with B carried and no stage", st)
	}
	fails("C starting", "local attach must be on or off")
	w.stopped()
	if st := status(); st.State != "stopped" || st.StartRefusal == nil || st.Stage != "" {
		t.Fatalf("C interrupted before it connected = %+v, want a stop that keeps B", st)
	}
	if d := stage(t, fails("C stopped", "local attach"), "status").Detail; strings.Contains(d, "no socket") || strings.Contains(d, "stage engine") {
		t.Errorf("B's remedy was replaced by A's: %q", d)
	}

	// A retry whose connect fails keeps B on disk; a hard kill (no stop) and the next start still show it.
	w, l = start()
	l.connectFailed(dnsFailure())
	if st := status(); st.StartRefusal == nil || st.Stage != types.RunnerStageDNS {
		t.Fatalf("after a failed connect = %+v, want B kept beside the connection failure's stage", st)
	}
	fails("after a failed connect", "local attach")
	_ = w // killed: no stopped()
	start()
	if st := status(); st.State != "starting" || st.StartRefusal == nil {
		t.Fatalf("after a hard restart = %+v, want B carried", st)
	}
	fails("after a hard restart", "local attach")

	// An old drop followed by a clean stop is still no failure: only a start refusal makes one.
	r.writeStatus(t, "", runnerStatus{State: "stopped", Since: statusAt, LastError: "link down", LastErrorAt: &statusAt})
	if s := stage(t, r.run(doctorOptions{}), "status"); s.Status == doctor.StatusFail {
		t.Errorf("a clean stop after an old error failed: %q", s.Detail)
	}
	_ = os.Remove(statusPath(state))

	// A newer refusal at a stage replaces B.
	w, l = start()
	l.startFailed(refusal)
	w.stopped()
	w, l = start()
	l.startFailed(atStage(types.RunnerStageEngine, errors.New("engine: no socket")))
	w.stopped()
	if st := status(); st.StartRefusal != nil || st.Stage != types.RunnerStageEngine || strings.Contains(st.LastError, "local attach") {
		t.Errorf("a staged refusal after B: %+v, want only the stage's", st)
	}

	// A start that connects clears it, and the stop after it stays clean.
	w, l = start()
	l.startFailed(refusal)
	w.stopped()
	w, _ = start()
	w.linkChanged(true, "")
	if s := stage(t, r.run(doctorOptions{}), "status"); s.Status == doctor.StatusFail {
		t.Errorf("a connected start still fails: %q", s.Detail)
	}
	w.stopped()
	if s := stage(t, r.run(doctorOptions{}), "status"); s.Status == doctor.StatusFail {
		t.Errorf("a refusal outlived a successful start: %q", s.Detail)
	}
	if st := status(); st.StartRefusal != nil {
		t.Errorf("the start refusal stayed after a connect: %+v", st)
	}
}
