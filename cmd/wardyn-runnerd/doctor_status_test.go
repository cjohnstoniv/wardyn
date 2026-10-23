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
		{types.RunnerStageDNS, "WARDYN_RUNNER_ORG_URL"},
		{types.RunnerStageProxy, "HTTPS_PROXY"},
		{types.RunnerStageTLS, "WARDYN_TRUSTED_CA_FILE"},
		{types.RunnerStageHealthz, "WARDYN_RUNNER_ORG_URL"},
		{types.RunnerStageEngine, "DOCKER_HOST"},
		{types.RunnerStageProxyImage, "WARDYN_PROXY_IMAGE"},
		{types.RunnerStageRelay, "WARDYN_RELAY_LISTEN"},
		{"", "WARDYN_RUNNER_ORG_URL"},
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
