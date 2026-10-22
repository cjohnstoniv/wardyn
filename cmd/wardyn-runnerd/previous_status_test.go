// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An upgrade keeps the binary it replaces as wardyn-runnerd.previous: a first install leaves none, a rerun
// of the same binary changes nothing, an upgrade keeps the old one, and a second upgrade replaces it.
func TestSetupKeepsOnePreviousBinary(t *testing.T) {
	clearProxyEnv(t)
	f := newSetupFixture(t, "linux")
	p := mustPaths(t, f)
	prev := p.installed + ".previous"
	ship := func(version string) {
		t.Helper()
		if err := os.WriteFile(f.host.exe, []byte(version), 0o755); err != nil {
			t.Fatal(err)
		}
		f.flagless = true
		if out, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	want := func(installed, previous string) {
		t.Helper()
		if got := f.read(p.installed); got != installed {
			t.Errorf("installed binary = %q, want %q", got, installed)
		}
		b, err := os.ReadFile(prev)
		switch {
		case previous == "" && err == nil:
			t.Errorf("a %q rollback copy exists, want none", b)
		case previous != "" && string(b) != previous:
			t.Errorf("rollback copy = %q (%v), want %q", b, err, previous)
		}
	}
	ship("v1")
	want("v1", "")
	ship("v1")
	want("v1", "")
	ship("v2")
	want("v2", "v1")
	if st, err := os.Stat(prev); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Errorf("the rollback copy cannot run: %v %v", st, err)
	}
	ship("v3")
	want("v3", "v2")
	if _, err := os.Stat(prev + ".tmp"); err == nil {
		t.Error("a temporary link was left beside the rollback copy")
	}
}

func readStatus(t *testing.T, dir string) (runnerStatus, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, statusFile))
	if err != nil {
		t.Fatal(err)
	}
	var st runnerStatus
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st, string(b)
}

// status.json records the link state and the last error with its time, mode 0600, and holds no secret: a
// proxy credential inside an error is cut out.
func TestStatusFileHoldsTheLinkStateAndNoSecret(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	w := newStatusWriter(dir)
	w.now = func() time.Time { return now }
	if st, _ := readStatus(t, dir); st.State != "starting" || st.PID != os.Getpid() {
		t.Errorf("at start: %+v", st)
	}
	w.linkChanged(false, `Get "https://org.example/stream": proxyconnect tcp: dial http://user:Pw-secret-31@proxy.corp:3128: connection refused`)
	st, raw := readStatus(t, dir)
	if st.State != "offline" || st.LastErrorAt == nil || !st.LastErrorAt.Equal(now) || !strings.Contains(st.LastError, "connection refused") {
		t.Errorf("after a failed attempt: %+v", st)
	}
	if strings.Contains(raw, "Pw-secret-31") || strings.Contains(raw, "user:") {
		t.Errorf("status.json holds a proxy credential:\n%s", raw)
	}
	now = now.Add(time.Minute)
	w.linkChanged(true, "")
	st, _ = readStatus(t, dir)
	if st.State != "connected" || !st.Since.Equal(now) || st.LastError == "" {
		t.Errorf("once connected the last error stays, with the state's own start: %+v", st)
	}
	w.stopped()
	if st, _ = readStatus(t, dir); st.State != "stopped" {
		t.Errorf("at exit: %+v", st)
	}
	if fi, err := os.Stat(filepath.Join(dir, statusFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("status.json mode: %v %v", fi, err)
	}
	for _, k := range []string{"token", "key", "password", "secret"} {
		if strings.Contains(strings.ToLower(raw), `"`+k) {
			t.Errorf("status.json has a field named like %q:\n%s", k, raw)
		}
	}
}
