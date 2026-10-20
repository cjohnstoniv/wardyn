// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearProxyEnv unsets every proxy variable the shell running the tests may hold.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range append(append([]string{}, carriedProxyVars...), "ALL_PROXY", "all_proxy") {
		t.Setenv(k, "")
	}
}

func TestCarryProxyStripsTheUserNameAndPassword(t *testing.T) {
	for in, want := range map[string]string{
		"http://alice:s3cr3t@proxy.corp:3128":    "http://proxy.corp:3128",
		"https://alice@proxy.corp":               "https://proxy.corp",
		"http://:onlypw@proxy.corp:8080":         "http://proxy.corp:8080",
		"alice:s3cr3t@proxy.corp:3128":           "proxy.corp:3128",
		"http://alice:p@ss@proxy.corp:3128":      "http://proxy.corp:3128",
		"http://alice:12/ss@proxy.corp:3128":     "http://proxy.corp:3128", // a password with an unescaped slash
		"http://proxy.corp:3128":                 "http://proxy.corp:3128",
		"socks5://alice:s3cr3t@proxy.corp:1080/": "socks5://proxy.corp:1080/",
	} {
		env := map[string]string{"HTTPS_PROXY": in}
		got, stripped, err := carryProxy(func(k string) string { return env[k] })
		if err != nil || len(got) != 1 || got[0].value != want {
			t.Errorf("carryProxy(%q) = %v, %v; want %q", in, got, err, want)
		}
		if has := strings.Contains(in, "@"); has != (len(stripped) == 1) {
			t.Errorf("carryProxy(%q) stripped=%v", in, stripped)
		}
	}
}

func TestCarryProxyCarriesOnlyWhatTheShellSetAndAddsNoNoProxy(t *testing.T) {
	env := map[string]string{"http_proxy": "http://proxy.corp:3128", "NO_PROXY": "localhost, .corp,10.0.0.0/8", "ALL_PROXY": "http://all.corp:1"}
	got, _, err := carryProxy(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, c := range got {
		names = append(names, c.name)
	}
	if strings.Join(names, " ") != "http_proxy NO_PROXY" {
		t.Errorf("carried %v; want http_proxy and NO_PROXY, in that order, and nothing the shell did not set", names)
	}
	none, _, err := carryProxy(func(string) string { return "" })
	if err != nil || len(none) != 0 {
		t.Errorf("a shell with no proxy carried %v, %v", none, err)
	}
}

// A value setup cannot write is refused naming the variable and never the value.
func TestCarryProxyRefusesWithoutRepeatingTheValue(t *testing.T) {
	for _, env := range []map[string]string{
		{"HTTPS_PROXY": "http://alice:topsecretpw@"},        // no host left once the credential goes
		{"HTTPS_PROXY": "http://alice:topsecretpw@h:80/$x"}, // a character a unit file would expand
		{"NO_PROXY": "a.corp\ntopsecretpw"},
		{"NO_PROXY": "alice:topsecretpw@a.corp"},
	} {
		_, _, err := carryProxy(func(k string) string { return env[k] })
		if err == nil || strings.Contains(err.Error(), "topsecretpw") || strings.Contains(err.Error(), "alice") {
			t.Errorf("carryProxy(%v) = %v; want a refusal that does not repeat the value", env, err)
		}
		for k := range env {
			if err != nil && !strings.Contains(err.Error(), k) {
				t.Errorf("the refusal %q does not name %s", err, k)
			}
		}
	}
}

// capturedLogs sends the default logger into the returned buffer for the test.
func capturedLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// The owner's rule: values never appear, only names. The password planted in the shell's HTTPS_PROXY is in
// no file setup wrote (the unit, the LaunchAgent, the environment file), not on stdout, not in the log and
// not in a command it ran; the service still gets the proxy without it.
func TestSetupCarriesTheShellProxyWithoutItsPassword(t *testing.T) {
	const pw, user = "Zx9-planted-pw-4471", "proxy-user-77"
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			clearProxyEnv(t)
			logs := capturedLogs(t)
			t.Setenv("HTTPS_PROXY", "http://"+user+":"+pw+"@proxy.corp:3128")
			t.Setenv("http_proxy", "http://"+user+":"+pw+"@proxy.corp:3128")
			t.Setenv("NO_PROXY", "localhost,.corp")
			f := newSetupFixture(t, goos)
			out, err := f.run(setupOptions{tokenFile: f.tokenFile})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			p := mustPaths(t, f)
			var service string
			if goos == "linux" {
				service = f.read(p.envFile)
				for _, want := range []string{"HTTPS_PROXY=http://proxy.corp:3128\n", "http_proxy=http://proxy.corp:3128\n", "NO_PROXY=localhost,.corp\n"} {
					if !strings.Contains(service, want) {
						t.Errorf("the env file lacks %q:\n%s", want, service)
					}
				}
			} else {
				service = f.read(p.plistFile)
				for _, want := range []string{"<key>HTTPS_PROXY</key>\n    <string>http://proxy.corp:3128</string>", "<key>http_proxy</key>\n    <string>http://proxy.corp:3128</string>", "<key>NO_PROXY</key>\n    <string>localhost,.corp</string>"} {
					if !strings.Contains(service, want) {
						t.Errorf("the LaunchAgent lacks %q:\n%s", want, service)
					}
				}
			}
			for _, absent := range []string{"<key>HTTP_PROXY</key>", "<key>https_proxy</key>", "<key>no_proxy</key>", "\nHTTP_PROXY=", "\nhttps_proxy="} {
				if strings.Contains(service, absent) {
					t.Errorf("the service carries %q, which the shell did not set:\n%s", absent, service)
				}
			}
			if strings.Count(out, "The runner service now carries HTTPS_PROXY, http_proxy, NO_PROXY from this shell") != 1 ||
				!strings.Contains(out, "without the user name and password it held; a proxy that needs them will refuse it (wardyn doctor shows it). Wardyn does not store proxy passwords.") {
				t.Errorf("setup did not print the one line about the proxy:\n%s", out)
			}
			// Nothing under the home directory, no output, no log and no command holds either secret.
			everything := out + logs.String() + strings.Join(f.commands, "\n")
			err = filepath.WalkDir(f.host.home, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				b, err := os.ReadFile(path)
				everything += "\n" + string(b)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(everything, pw) || strings.Contains(everything, user) {
				t.Errorf("a proxy user name or password reached a file, the output, the log or a command")
			}
		})
	}
}

// With nothing set in the shell the service gets no proxy and no NO_PROXY default.
func TestSetupCarriesNoProxyWhenTheShellHasNone(t *testing.T) {
	clearProxyEnv(t)
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	env := f.read(mustPaths(t, f).envFile)
	if strings.Contains(strings.ToUpper(env), "_PROXY=") || strings.Contains(out, "now carries") {
		t.Errorf("setup carried a proxy the shell did not set:\n%s\n%s", env, out)
	}
}

// A proxy value setup cannot carry is refused before the token is spent, naming the variable only.
func TestSetupRefusesAnUncarriableProxyBeforeSpendingTheToken(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://u:planted-pw-1@h:80/$x")
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err == nil || !strings.Contains(err.Error(), "HTTPS_PROXY") || strings.Contains(err.Error()+out, "planted-pw-1") {
		t.Fatalf("err=%v out=%s; want a refusal naming HTTPS_PROXY and not its value", err, out)
	}
	if f.count("POST /api/v1/runners/register") != 0 {
		t.Error("the token was spent")
	}
}
