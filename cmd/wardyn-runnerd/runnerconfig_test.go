// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func inputs(flags, env, managed map[string]string, file runnerFile) runnerInputs {
	return runnerInputs{
		flag:     func(n string) (string, bool) { v, ok := flags[n]; return v, ok },
		getenv:   func(k string) string { return env[k] },
		managed:  func(k string) string { return managed[k] },
		file:     file,
		filePath: "/cfg/runnerd.json",
	}
}

// One test per level of the order: a level wins over every one below it.
func TestRunnerConfigPrecedence(t *testing.T) {
	file := runnerFile{InternalNetwork: "from-file"}
	env := map[string]string{"WARDYN_INTERNAL_NETWORK": "from-env"}
	managed := map[string]string{"InternalNetwork": "from-managed"}
	flags := map[string]string{"internal-network": "from-flag"}
	for _, tc := range []struct {
		level, want, source string
		in                  runnerInputs
	}{
		{"flag", "from-flag", "flag --internal-network", inputs(flags, env, managed, file)},
		{"managed preference", "from-managed", "managed preference InternalNetwork", inputs(nil, env, managed, file)},
		{"environment", "from-env", "environment WARDYN_INTERNAL_NETWORK", inputs(nil, env, nil, file)},
		{"file", "from-file", "file /cfg/runnerd.json", inputs(nil, nil, nil, file)},
		{"default", "wardyn-internal", "default", inputs(nil, nil, nil, runnerFile{})},
	} {
		t.Run(tc.level, func(t *testing.T) {
			got := resolveRunnerConfig(tc.in).field("internal_network")
			if got.Value != tc.want || got.Source != tc.source {
				t.Fatalf("internal_network = %q from %q, want %q from %q", got.Value, got.Source, tc.want, tc.source)
			}
		})
	}
}

// org_ca_file follows the same order as every other field, one case per level.
func TestRunnerConfigOrgCAFileOrder(t *testing.T) {
	file := runnerFile{OrgCAFile: "/from/file.pem"}
	env := map[string]string{"WARDYN_ORG_CA_FILE": "/from/env.pem"}
	managed := map[string]string{"OrgCAFile": "/from/managed.pem"}
	flags := map[string]string{"org-ca-file": "/from/flag.pem"}
	for _, tc := range []struct {
		level, want string
		in          runnerInputs
	}{
		{"flag", "/from/flag.pem", inputs(flags, env, managed, file)},
		{"managed preference", "/from/managed.pem", inputs(nil, env, managed, file)},
		{"environment", "/from/env.pem", inputs(nil, env, nil, file)},
		{"file", "/from/file.pem", inputs(nil, nil, nil, file)},
		{"default", "", inputs(nil, nil, nil, runnerFile{})},
	} {
		if got := resolveRunnerConfig(tc.in).get("org_ca_file"); got != tc.want {
			t.Errorf("%s: org_ca_file = %q, want %q", tc.level, got, tc.want)
		}
	}
	if _, err := loadRunnerFile(writeConfig(t, `{"org_ca_file": "relative.pem"}`), true); err == nil || !strings.Contains(err.Error(), "org_ca_file") {
		t.Errorf("a relative org_ca_file: %v; want a refusal naming it", err)
	}
}

// Every field is reachable at every level it has, and an unset one is empty.
func TestRunnerConfigEveryFieldResolvesFromEachSource(t *testing.T) {
	for _, f := range runnerFields {
		var file runnerFile
		*f.file(&file) = "file-value"
		if got := resolveRunnerConfig(inputs(nil, nil, nil, file)).field(f.name); got.Value != "file-value" || !strings.HasPrefix(got.Source, "file ") {
			t.Errorf("%s: from the file = %+v", f.name, got)
		}
		if f.flag != "" {
			if got := resolveRunnerConfig(inputs(map[string]string{f.flag: "flag-value"}, nil, nil, file)).field(f.name); got.Value != "flag-value" {
				t.Errorf("%s: --%s lost to the file: %+v", f.name, f.flag, got)
			}
		}
		for _, e := range f.env {
			if got := resolveRunnerConfig(inputs(nil, map[string]string{e: "env-value"}, nil, file)).field(f.name); got.Value != "env-value" || got.Source != "environment "+e {
				t.Errorf("%s: %s lost to the file: %+v", f.name, e, got)
			}
		}
		if f.managed != "" {
			if got := resolveRunnerConfig(inputs(nil, nil, map[string]string{f.managed: "managed-value"}, file)).field(f.name); got.Value != "managed-value" {
				t.Errorf("%s: the %s preference lost to the file: %+v", f.name, f.managed, got)
			}
		}
	}
}

// An install that has only environment variables, as every setup before runnerd.json left it, resolves
// the way it did: each variable is the value, and the defaults fill the rest.
func TestRunnerConfigEnvironmentOnlyInstallGolden(t *testing.T) {
	env := map[string]string{
		"DOCKER_HOST": "unix:///run/user/1000/docker.sock", "WARDYN_PROXY_IMAGE": "mirror.example/wardyn-proxy:0.10.0",
		"WARDYN_RELAY_LISTEN": "127.0.0.1", "WARDYN_RELAY_HOST_TARGET": "host-gateway", "WARDYN_TRUSTED_CA_FILE": "/etc/wardyn/ca.pem",
		"WARDYN_INTERNAL_NETWORK": "corp-net", "WARDYN_UPSTREAM_PROXY": "http://proxy.example:3128", "HTTPS_PROXY": "http://proxy.example:3128",
	}
	var got strings.Builder
	for _, f := range resolveRunnerConfig(inputs(nil, env, nil, runnerFile{})) {
		fmt.Fprintf(&got, "%s=%s (%s)\n", f.Name, f.Value, f.Source)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "runnerconfig_env_only.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("effective config of an environment-only install changed:\n%s\nwant:\n%s", got.String(), want)
	}
}

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	return writeFile(t, filepath.Join(t.TempDir(), runnerConfigName), text)
}

func TestRunnerFileRefusesAProxyCredential(t *testing.T) {
	for _, tc := range []struct{ field, env string }{{"upstream_proxy", "WARDYN_UPSTREAM_PROXY"}, {"runner_proxy", "HTTPS_PROXY"}} {
		path := writeConfig(t, `{"`+tc.field+`": "http://alice:hunter2@proxy.example:3128"}`)
		_, err := loadRunnerFile(path, true)
		if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "set "+tc.env+" in the service's environment") {
			t.Errorf("%s with userinfo: %v; want a refusal naming the field and %s", tc.field, err, tc.env)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the refusal repeats the credential: %v", tc.field, err)
		}
		if _, err := loadRunnerFile(writeConfig(t, `{"`+tc.field+`": "http://proxy.example:3128"}`), true); err != nil {
			t.Errorf("%s without userinfo: %v", tc.field, err)
		}
	}
	if err := writeRunnerFile(filepath.Join(t.TempDir(), runnerConfigName), runnerFile{UpstreamProxy: "http://u:p@proxy.example:3128"}); err == nil {
		t.Error("setup's writer accepted a credential")
	}
}

// A scheme-less value is read the way Go reads HTTPS_PROXY (as http://<value>), so a credential written
// without a scheme is refused and a bare host:port keeps working; a scheme the runner cannot use is refused by name.
func TestRunnerProxyWithoutASchemeIsReadAsHTTP(t *testing.T) {
	for _, field := range []string{"runner_proxy", "upstream_proxy"} {
		_, err := loadRunnerFile(writeConfig(t, `{"`+field+`": "alice:hunter2@proxy.example:3128"}`), true)
		if err == nil || !strings.Contains(err.Error(), "user name or password") || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: scheme-less userinfo: %v; want the credential refusal without the value", field, err)
		}
	}
	if _, err := loadRunnerFile(writeConfig(t, `{"runner_proxy": "proxy.example:3128"}`), true); err != nil {
		t.Errorf("runner_proxy bare host:port: %v", err)
	}
	for _, ok := range []string{"http://p:1", "https://p:1", "socks5://p:1", "socks5h://p:1"} {
		if _, err := loadRunnerFile(writeConfig(t, `{"runner_proxy": "`+ok+`"}`), true); err != nil {
			t.Errorf("runner_proxy %s: %v", ok, err)
		}
	}
	_, err := loadRunnerFile(writeConfig(t, `{"runner_proxy": "ftp://proxy.example:21"}`), true)
	if err == nil || !strings.Contains(err.Error(), `"ftp"`) {
		t.Errorf("runner_proxy ftp: %v; want a refusal naming the scheme", err)
	}
}

// A flag or an environment value that is only whitespace is unset, the way the old serve trimmed it.
func TestRunnerConfigWhitespaceOnlyIsUnset(t *testing.T) {
	got := resolveRunnerConfig(inputs(map[string]string{"upstream-proxy": "  "}, map[string]string{"WARDYN_UPSTREAM_PROXY": " http://p:1 "}, nil, runnerFile{})).field("upstream_proxy")
	if got.Value != "http://p:1" || got.Source != "environment WARDYN_UPSTREAM_PROXY" {
		t.Errorf("blank flag: %+v; want the environment's trimmed value", got)
	}
	got = resolveRunnerConfig(inputs(map[string]string{"upstream-proxy": " http://f:1 "}, nil, nil, runnerFile{})).field("upstream_proxy")
	if got.Value != "http://f:1" {
		t.Errorf("padded flag: %+v; want it trimmed", got)
	}
}

func TestRunnerFileRefusesAnUnknownField(t *testing.T) {
	_, err := loadRunnerFile(writeConfig(t, `{"org_url": "https://wardyn.example", "relay_adress": "127.0.0.1"}`), true)
	if err == nil || !strings.Contains(err.Error(), "relay_adress") {
		t.Fatalf("an unknown field: %v; want an error naming it", err)
	}
}

func TestRunnerFileNamesEachBadField(t *testing.T) {
	for field, text := range map[string]string{
		"org_url":            `{"org_url": "ftp://x"}`,
		"state_dir":          `{"state_dir": "relative/dir"}`,
		"proxy_image_mirror": `{"proxy_image_mirror": "registry.example/wardyn-proxy:0.10.0"}`,
		"recording_store":    `{"recording_store": "disk"}`,
		"relay_listen":       `{"relay_listen": "not an address"}`,
	} {
		if _, err := loadRunnerFile(writeConfig(t, text), true); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v; want an error naming the field", field, err)
		}
	}
}

func TestRunnerFileAbsentIsEmptyUnlessNamed(t *testing.T) {
	path := filepath.Join(t.TempDir(), runnerConfigName)
	if f, err := loadRunnerFile(path, false); err != nil || f != (runnerFile{}) {
		t.Fatalf("the default file, absent: %+v %v", f, err)
	}
	if _, err := loadRunnerFile(path, true); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("a named file, absent: %v", err)
	}
}

func TestRunnerFileIsWrittenPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wardyn", runnerConfigName)
	if err := writeRunnerFile(path, runnerFile{OrgURL: "https://wardyn.example", InternalNetwork: "wardyn-internal"}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("runnerd.json mode = %v (%v), want 0600", st, err)
	}
	if got, err := loadRunnerFile(path, true); err != nil || got.OrgURL != "https://wardyn.example" {
		t.Fatalf("read back: %+v %v", got, err)
	}
}

// The service definitions name the binary and the config path and carry no setting value, whatever the
// plan holds.
func TestServiceDefinitionsCarryNoSettings(t *testing.T) {
	paths, err := setupHost{goos: "darwin", home: "/Users/me"}.paths()
	if err != nil {
		t.Fatal(err)
	}
	plan := setupPlan{setupOptions: setupOptions{org: "https://wardyn.example", proxyImage: "mirror.example/wardyn-proxy:1", network: "corp-net",
		relayListen: "127.0.0.1", relayHostTarget: "192.168.5.2", trustedCAFile: "/etc/wardyn/ca.pem"}, paths: paths, flagged: true, dockerHost: "unix:///run/docker.sock"}
	linux, err := setupHost{goos: "linux", home: "/home/me"}.paths()
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"unit": renderSystemdUnit(linux), "launchagent": renderLaunchAgent(plan)} {
		for _, secret := range []string{"wardyn.example", "mirror.example", "corp-net", "127.0.0.1", "192.168.5.2", "/etc/wardyn/ca.pem", "docker.sock", "EnvironmentVariables", "EnvironmentFile"} {
			if strings.Contains(got, secret) {
				t.Errorf("the %s carries %q", name, secret)
			}
		}
		want, err := os.ReadFile(filepath.Join("testdata", "service_"+name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("the %s changed:\n%s", name, got)
		}
	}
}

func TestSetupKeepsWhatWasEditedByHandInRunnerFile(t *testing.T) {
	setupBuild(t, "0.9.1")
	f := newSetupFixture(t, "linux")
	p := mustPaths(t, f)
	if err := writeRunnerFile(p.configFile(), runnerFile{ProxyImageMirror: "mirror.example/wardyn-proxy", UpstreamProxy: "http://proxy.example:3128", RecordingStore: "fs"}); err != nil {
		t.Fatal(err)
	}
	f.flagless = true
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
		t.Fatal(err)
	}
	got, err := loadRunnerFile(p.configFile(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyImageMirror != "mirror.example/wardyn-proxy" || got.UpstreamProxy != "http://proxy.example:3128" || got.RecordingStore != "fs" || got.OrgURL != f.org {
		t.Fatalf("setup dropped a hand-edited setting or lost its own: %+v", got)
	}
	// The probe pulled the image from the mirror, at this release's tag.
	if want := "mirror.example/wardyn-proxy:0.9.1 wardyn-internal"; len(f.checks) == 0 || f.checks[len(f.checks)-1] != want {
		t.Errorf("probed %q, want the last to be %q", f.checks, want)
	}
}

func TestSetupRefusesAnInvalidRunnerFileBeforeSpendingTheToken(t *testing.T) {
	f := newSetupFixture(t, "linux")
	p := mustPaths(t, f)
	if err := os.MkdirAll(filepath.Dir(p.configFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.configFile(), `{"relay_adress": "x"}`)
	_, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err == nil || !strings.Contains(err.Error(), "relay_adress") || !strings.Contains(err.Error(), "no token was spent") || len(f.calls) != 0 {
		t.Fatalf("an invalid runnerd.json: err=%v calls=%v", err, f.calls)
	}
}

// An install an earlier setup made keeps its pins when setup runs again, and they move out of the
// service definition (on macOS a plist every local account reads) into runnerd.json.
func TestSetupMovesALegacyServiceDefinitionIntoRunnerFile(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSetupFixture(t, goos)
			f.flagless = true
			p := mustPaths(t, f)
			ca := writeFile(t, filepath.Join(t.TempDir(), "corp-ca.pem"), testCAPEM(t, "corp"))
			if _, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(p.configFile()); err != nil {
				t.Fatal(err)
			}
			legacy, path := "WARDYN_PROXY_IMAGE=mirror.example/wardyn-proxy:7\nWARDYN_PROXY_IMAGE_PINNED=1\nWARDYN_TRUSTED_CA_FILE="+ca+"\n", p.noServiceEnvFile()
			if goos == "darwin" {
				path = p.plistFile
				legacy = "<plist><dict><key>EnvironmentVariables</key><dict><key>WARDYN_PROXY_IMAGE</key>\n<string>mirror.example/wardyn-proxy:7</string>\n" +
					"<key>WARDYN_PROXY_IMAGE_PINNED</key>\n<string>1</string><key>WARDYN_TRUSTED_CA_FILE</key>\n<string>" + ca + "</string></dict></dict></plist>"
			}
			writeFile(t, path, legacy)
			if _, err := f.run(setupOptions{}); err != nil {
				t.Fatal(err)
			}
			got := f.settings(p)
			for _, want := range []string{"WARDYN_PROXY_IMAGE=mirror.example/wardyn-proxy:7\n", "WARDYN_TRUSTED_CA_FILE=" + ca + "\n"} {
				if !strings.Contains(got, want) {
					t.Errorf("the pin %q did not move into runnerd.json:\n%s", want, got)
				}
			}
			if goos == "linux" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("the old environment file stayed: %v", err)
				}
			} else if plist := f.read(path); strings.Contains(plist, "mirror.example") || strings.Contains(plist, ca) || strings.Contains(plist, "EnvironmentVariables") {
				t.Errorf("the LaunchAgent still carries the settings:\n%s", plist)
			}
		})
	}
}

// serve validates the file at startup, naming the field, before it reads an identity or touches Docker.
func TestServeRefusesABadRunnerFile(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{`{"org_url": "https://org.example", "bogus": 1}`, "bogus"},
		{`{"org_url": "https://org.example", "upstream_proxy": "http://a:b@proxy.example:3128"}`, "upstream_proxy"},
	} {
		built := false
		cmd := newServeCommand(func(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
			built = true
			return runnerRuntime{}, nil
		})
		cmd.SetArgs([]string{"--config", writeConfig(t, tc.text), "--state-dir", t.TempDir()})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) || built {
			t.Errorf("%s: err=%v built=%v; want a refusal naming %q before the runner is built", tc.text, err, built, tc.want)
		}
	}
	cmd := newServeCommand(nil)
	cmd.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "absent.json")})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("a --config that does not exist: %v", err)
	}
}

// serve runs on the file alone: the organisation, the state directory, the engine, the proxy and the mirror.
func TestServeRunsOnRunnerFile(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	state := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(state)
	if err != nil {
		t.Fatal(err)
	}
	id := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)),
		OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), ProxyImage: "ghcr.io/cjohnstoniv/wardyn-proxy:0.10.0"}
	if err := runneridentity.Save(state, id); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, `{"org_url": "https://org.example", "state_dir": "`+state+`", "docker_host": "unix:///run/test-engine.sock",
		"runner_proxy": "http://proxy.example:3128", "proxy_image_mirror": "mirror.example/wardyn-proxy", "recording_store": "off"}`)
	var got serveOptions
	var host, proxyEnv string
	cmd := newServeCommand(func(_ context.Context, _ runneridentity.Identity, dir string, o serveOptions) (runnerRuntime, error) {
		got, host, proxyEnv = o, os.Getenv("DOCKER_HOST"), os.Getenv("HTTPS_PROXY")
		if dir != state {
			t.Errorf("state dir %q, want %q", dir, state)
		}
		return runnerRuntime{}, nil
	})
	cmd.SetArgs([]string{"--config", cfg})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	_ = cmd.Execute()
	if host != "unix:///run/test-engine.sock" || proxyEnv != "http://proxy.example:3128" {
		t.Errorf("DOCKER_HOST=%q HTTPS_PROXY=%q; the file's engine and proxy did not reach the process", host, proxyEnv)
	}
	if got.recording != "off" || got.network != "wardyn-internal" || got.proxyImageMirror != "mirror.example/wardyn-proxy" || got.proxyImagePinned {
		t.Errorf("options from the file: %+v", got)
	}
	if want := "mirror.example/wardyn-proxy:0.10.0"; got.proxyImage != want {
		t.Errorf("proxy image %q, want the organisation's tag from the mirror, %q", got.proxyImage, want)
	}
}

func TestMirrorProxyImageFollowsTheTag(t *testing.T) {
	const m = "mirror.example:5000/wardyn-proxy"
	for in, want := range map[string]string{
		"ghcr.io/cjohnstoniv/wardyn-proxy:0.10.0":                   m + ":0.10.0",
		"ghcr.io/x/wardyn-proxy:1@sha256:" + strings.Repeat("a", 8): m + ":1@sha256:" + strings.Repeat("a", 8),
		"ghcr.io/x/wardyn-proxy@sha256:" + strings.Repeat("a", 8):   m + "@sha256:" + strings.Repeat("a", 8),
		"localhost:5000/wardyn-proxy":                               "localhost:5000/wardyn-proxy",
		"":                                                          "",
	} {
		if got := mirrorProxyImage(m, in); got != want {
			t.Errorf("mirror of %q = %q, want %q", in, got, want)
		}
	}
	if got := mirrorProxyImage("", "ghcr.io/x/wardyn-proxy:1"); got != "ghcr.io/x/wardyn-proxy:1" {
		t.Errorf("no mirror changed the image: %q", got)
	}
}

func TestMirroredProxyImageHookRewritesOnlyAnUnpinnedRunner(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		var got string
		mirroredProxyImageHook("mirror.example/wardyn-proxy", pinned, func(a string) { got = a })("ghcr.io/cjohnstoniv/wardyn-proxy:0.10.1")
		if want := map[bool]string{false: "mirror.example/wardyn-proxy:0.10.1", true: "ghcr.io/cjohnstoniv/wardyn-proxy:0.10.1"}[pinned]; got != want {
			t.Errorf("pinned=%v: the hook passed %q, want %q", pinned, got, want)
		}
	}
}
