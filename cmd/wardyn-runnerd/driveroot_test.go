// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
)

// withManagedDriveRoot makes this a Mac whose profile holds DriveRoot, which plutil prints as JSON.
func withManagedDriveRoot(t *testing.T, json string) {
	t.Helper()
	oldOS, oldExtract, oldJSON := prefsOS, prefsExtract, prefsExtractJSON
	t.Cleanup(func() { prefsOS, prefsExtract, prefsExtractJSON = oldOS, oldExtract, oldJSON })
	prefsOS = "darwin"
	prefsExtract = func(string, string) (string, error) { return "", nil }
	prefsExtractJSON = func(_, key string) (string, error) {
		if key == "DriveRoot" {
			return json, nil
		}
		return "", nil
	}
}

// serveCmdConfig is serve's configuration from args and, when fileJSON is set, a runnerd.json holding it.
func serveCmdConfig(t *testing.T, fileJSON string, args ...string) (runnerConfig, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WARDYN_DRIVE_ROOT", "")
	path := ""
	if fileJSON != "" {
		path = writeConfig(t, fileJSON)
	}
	cmd := newServeCommand(nil)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return serveConfig(cmd, path)
}

// drive_root resolves flag > managed preference > environment > runnerd.json, and says where it came from.
func TestDriveRootResolutionOrder(t *testing.T) {
	file := runnerFile{DriveRoot: []string{"/from/file"}}
	env := map[string]string{"WARDYN_DRIVE_ROOT": "/from/env" + string(filepath.ListSeparator) + "/from/env2"}
	managed := map[string]string{"DriveRoot": `["/from/managed","/from/managed2"]`}
	flag := func(string) ([]string, bool) { return []string{"/from/flag", "/from/flag2"}, true }
	with := func(flag func(string) ([]string, bool), env, managed map[string]string, file runnerFile) runnerInputs {
		in := inputs(nil, env, managed, file)
		in.flagList = flag
		return in
	}
	for _, tc := range []struct {
		level  string
		in     runnerInputs
		want   []string
		source string
	}{
		{"flag", with(flag, env, managed, file), []string{"/from/flag", "/from/flag2"}, "flag --drive-root"},
		{"managed preference", with(nil, env, managed, file), []string{"/from/managed", "/from/managed2"}, "managed preference DriveRoot"},
		{"environment", with(nil, env, nil, file), []string{"/from/env", "/from/env2"}, "environment WARDYN_DRIVE_ROOT"},
		{"file", with(nil, nil, nil, file), []string{"/from/file"}, "file /cfg/runnerd.json"},
		{"default", with(nil, nil, nil, runnerFile{}), nil, "default"},
	} {
		got := resolveRunnerConfig(tc.in).field("drive_root")
		if !reflect.DeepEqual(got.List, tc.want) || got.Source != tc.source || got.Err != nil {
			t.Errorf("%s: drive_root = %+v, want %v from %q", tc.level, got, tc.want, tc.source)
		}
	}
}

// A --drive-root the person gave wins over every other source: changedFlags reads only string flags, and this
// one is repeatable.
func TestDriveRootFlagWinsOverEverySource(t *testing.T) {
	withManagedDriveRoot(t, `["/from/managed"]`)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WARDYN_DRIVE_ROOT", "/from/env")
	path := writeConfig(t, `{"drive_root": ["/from/file"]}`)
	for _, tc := range []struct {
		args []string
		want []string
		src  string
	}{
		{[]string{"--drive-root", "/from/flag", "--drive-root", "/from/flag2"}, []string{"/from/flag", "/from/flag2"}, "flag --drive-root"},
		{nil, []string{"/from/managed"}, "managed preference DriveRoot"},
	} {
		cmd := newServeCommand(nil)
		if err := cmd.ParseFlags(tc.args); err != nil {
			t.Fatal(err)
		}
		cfg, err := serveConfig(cmd, path)
		if err != nil {
			t.Fatal(err)
		}
		var org, state string
		var opts serveOptions
		cfg.applyTo(&org, &state, &opts)
		if got := cfg.field("drive_root"); !reflect.DeepEqual(opts.driveRoots, tc.want) || got.Source != tc.src {
			t.Errorf("serve %v: roots %v from %q, want %v from %q", tc.args, opts.driveRoots, got.Source, tc.want, tc.src)
		}
	}
}

// A drive root serve would refuse is refused from every source, and the refusal names the source.
func TestDriveRootRefusalsNameTheirSource(t *testing.T) {
	for _, bad := range []string{"relative/dir", "/a/../b", "/a/", "/", ""} {
		for _, tc := range []struct {
			source string
			config func(t *testing.T) error
			want   string
		}{
			{"flag", func(t *testing.T) error {
				cfg, err := serveCmdConfig(t, "", "--drive-root", bad)
				if err != nil {
					return err
				}
				return cfg.checkDriveRoots("docker")
			}, `--drive-root "` + bad + `"`},
			{"environment", func(t *testing.T) error {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				cmd := newServeCommand(nil)
				t.Setenv("WARDYN_DRIVE_ROOT", bad)
				cfg, err := serveConfig(cmd, "")
				if err != nil {
					return err
				}
				return cfg.checkDriveRoots("docker")
			}, `drive_root (environment WARDYN_DRIVE_ROOT) "` + bad + `"`},
			{"managed preference", func(t *testing.T) error {
				withManagedDriveRoot(t, `["`+bad+`"]`)
				cfg, err := serveCmdConfig(t, "")
				if err != nil {
					return err
				}
				return cfg.checkDriveRoots("docker")
			}, `drive_root (managed preference DriveRoot) "` + bad + `"`},
			{"file", func(t *testing.T) error {
				_, err := serveCmdConfig(t, `{"drive_root": ["`+bad+`"]}`)
				return err
			}, `drive_root "` + bad + `"`},
		} {
			if bad == "" && tc.source == "environment" {
				continue // an empty variable is unset
			}
			t.Run(tc.source+" "+bad, func(t *testing.T) {
				if err := tc.config(t); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "give an absolute, cleaned folder below /") {
					t.Errorf("%v; want a refusal containing %s", err, tc.want)
				}
			})
		}
	}
	// A managed DriveRoot that is no array of strings is refused, not read as no roots.
	withManagedDriveRoot(t, `"/a/string"`)
	cfg, err := serveCmdConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.checkDriveRoots("docker"); err == nil || !strings.Contains(err.Error(), "DriveRoot") {
		t.Errorf("a managed DriveRoot that is a string: %v", err)
	}
}

// serve itself refuses a bad root from the environment, before it reads an identity or dials anything.
func TestServeRefusesABadDriveRootFromTheEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WARDYN_DRIVE_ROOT", "relative")
	cmd := newServeCommand(nil)
	cmd.SetArgs([]string{"--org", "https://wardyn.example.com", "--state-dir", t.TempDir()})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "WARDYN_DRIVE_ROOT") {
		t.Fatalf("serve with a relative WARDYN_DRIVE_ROOT = %v", err)
	}
}

// --drive-root applies to the docker substrate only, whichever source holds it.
func TestDriveRootOnlyOnDockerFromEverySource(t *testing.T) {
	for _, tc := range []struct {
		source string
		setup  func(t *testing.T) (args []string)
	}{
		{"flag", func(t *testing.T) []string { return []string{"--drive-root", "/srv/x"} }},
		{"environment", func(t *testing.T) []string { t.Setenv("WARDYN_DRIVE_ROOT", "/srv/x"); return nil }},
		{"managed preference", func(t *testing.T) []string { withManagedDriveRoot(t, `["/srv/x"]`); return nil }},
		{"file", func(t *testing.T) []string {
			return []string{"--config", writeConfig(t, `{"drive_root": ["/srv/x"]}`)}
		}},
	} {
		for _, sub := range []string{"k8s", "folders"} {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("WARDYN_DRIVE_ROOT", "")
			args := append(tc.setup(t), "--substrate", sub, "--org", "https://wardyn.example.com", "--state-dir", t.TempDir())
			cmd := newServeCommand(nil)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "applies only to --substrate=docker") {
				t.Errorf("--substrate %s with drive_root from the %s = %v, want the docker-only refusal", sub, tc.source, err)
			}
		}
	}
}

// setup --drive-root writes drive_root to runnerd.json; a rerun with it replaces the list, and a rerun without it
// keeps the list.
func TestSetupWritesDriveRoots(t *testing.T) {
	f := newSetupFixture(t, "linux")
	p, _ := f.host.paths()
	roots := func() []string {
		t.Helper()
		file, err := loadRunnerFile(p.configFile(), false)
		if err != nil {
			t.Fatal(err)
		}
		return file.DriveRoot
	}
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
		t.Fatal(err)
	}
	if got := roots(); len(got) != 0 {
		t.Fatalf("a setup without --drive-root wrote %v", got)
	}
	if _, err := f.run(setupOptions{driveRoots: []string{"/srv/a", "/srv/b"}}); err != nil {
		t.Fatal(err)
	}
	if got := roots(); !reflect.DeepEqual(got, []string{"/srv/a", "/srv/b"}) {
		t.Fatalf("setup --drive-root wrote %v", got)
	}
	if _, err := f.run(setupOptions{driveRoots: []string{"/srv/c"}}); err != nil {
		t.Fatal(err)
	}
	if got := roots(); !reflect.DeepEqual(got, []string{"/srv/c"}) {
		t.Fatalf("a rerun with --drive-root left %v, want it replaced by /srv/c", got)
	}
	f.flagless = true
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := roots(); !reflect.DeepEqual(got, []string{"/srv/c"}) {
		t.Fatalf("a rerun without --drive-root left %v, want /srv/c kept", got)
	}
}

// A drive root serve would refuse is refused by setup before the token is spent or a file is written.
func TestSetupRefusesABadDriveRoot(t *testing.T) {
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, driveRoots: []string{"relative"}})
	if err == nil || !strings.Contains(err.Error(), `drive_root "relative"`) || !strings.Contains(err.Error(), "no token was spent") {
		t.Fatalf("setup --drive-root relative = %v\n%s", err, out)
	}
	if n := f.count("POST /api/v1/runners/register"); n != 0 {
		t.Errorf("the token was spent: %d registrations", n)
	}
	// The command refuses it by the flag's own name, and a bad managed DriveRoot the same.
	cmd := newSetupCommand()
	cmd.SetArgs([]string{"--org", f.org, "--drive-root", "/a/../b"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `--drive-root "/a/../b"`) {
		t.Errorf("setup --drive-root /a/../b = %v", err)
	}
	withManagedDriveRoot(t, `["relative"]`)
	cmd = newSetupCommand()
	cmd.SetArgs([]string{"--org", f.org})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "DriveRoot") {
		t.Errorf("setup with a bad managed DriveRoot = %v", err)
	}
}

// doctor prints the effective roots and where they came from, the way it names other settings, and prints
// nothing when none are set.
func TestDoctorPrintsDriveRootsAndTheirSource(t *testing.T) {
	r := newRig(t)
	opts := doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0"}
	for _, s := range r.run(opts).Stages {
		if s.Name == "drive-roots" {
			t.Fatalf("no roots are set, but doctor printed %+v", s)
		}
	}
	r.writeRunnerJSON(t, runnerFile{DriveRoot: []string{"/srv/a", "/srv/b"}})
	if s := stage(t, r.run(opts), "drive-roots"); s.Status != doctor.StatusOK || s.Detail != "/srv/a, /srv/b (from drive_root in the service's settings)" {
		t.Errorf("from runnerd.json: %+v", s)
	}
	r.env["WARDYN_DRIVE_ROOT"] = "/srv/env"
	if s := stage(t, r.run(opts), "drive-roots"); s.Status != doctor.StatusOK || s.Detail != "/srv/env (from WARDYN_DRIVE_ROOT)" {
		t.Errorf("from the environment: %+v", s)
	}
	withManagedDriveRoot(t, `["/srv/managed"]`)
	if s := stage(t, r.run(opts), "drive-roots"); s.Status != doctor.StatusOK || s.Detail != "/srv/managed (from the DriveRoot managed preference)" {
		t.Errorf("from the managed preference: %+v", s)
	}
	withManagedDriveRoot(t, `["relative"]`)
	if s := stage(t, r.run(opts), "drive-roots"); s.Status != doctor.StatusFail || !strings.Contains(s.Detail, `"relative"`) || !strings.Contains(s.Detail, "managed preference DriveRoot") {
		t.Errorf("a bad managed root: %+v", s)
	}
}

// doctor judges drive_root against the substrate the runner serves, as serve does: on a Kubernetes or folders
// runner a drive_root from runnerd.json fails with serve's refusal, never reads as OK.
func TestDoctorRefusesDriveRootOffDocker(t *testing.T) {
	r := newRig(t)
	r.writeRunnerJSON(t, runnerFile{DriveRoot: []string{"/srv/a"}})
	for _, sub := range []string{"k8s", "folders"} {
		opts := doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0", substrate: sub}
		if s := stage(t, r.run(opts), "drive-roots"); s.Status != doctor.StatusFail || !strings.Contains(s.Detail, "applies only to --substrate=docker") {
			t.Errorf("--substrate %s: %+v, want serve's docker-only refusal", sub, s)
		}
	}
	opts := doctorOptions{proxyImage: releaseProxyRepo + ":0.10.0", substrate: "docker"}
	if s := stage(t, r.run(opts), "drive-roots"); s.Status != doctor.StatusOK {
		t.Errorf("--substrate docker: %+v, want OK", s)
	}
}
