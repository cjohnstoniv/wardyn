// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─── maskProxyURL ────────────────────────────────────────────────────────────

func TestMaskProxyURL(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantMasked string
		wantCred   bool
	}{
		{"userpass", "http://user:pass@proxy.corp:8080", "http://user:***@proxy.corp:8080", true},
		{"user_only", "http://user@proxy.corp:8080", "http://user@proxy.corp:8080", true},
		{"no_creds_scheme", "http://proxy.corp:8080", "http://proxy.corp:8080", false},
		{"no_creds_bare", "proxy.corp:8080", "proxy.corp:8080", false},
		{"bare_userpass", "user:pass@proxy.corp:8080", "user:***@proxy.corp:8080", true},
		{"no_proxy_list_untouched", "localhost,127.0.0.1,.corp.internal", "localhost,127.0.0.1,.corp.internal", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			masked, hasCred := maskProxyURL(tc.in)
			if masked != tc.wantMasked {
				t.Errorf("maskProxyURL(%q) value = %q, want %q", tc.in, masked, tc.wantMasked)
			}
			if hasCred != tc.wantCred {
				t.Errorf("maskProxyURL(%q) hasCred = %v, want %v", tc.in, hasCred, tc.wantCred)
			}
			// The raw password must never appear in the masked output.
			if tc.wantCred {
				parseTarget := tc.in
				if !strings.Contains(tc.in, "://") {
					parseTarget = "http://" + tc.in
				}
				u, _ := url.Parse(parseTarget)
				if pw, ok := u.User.Password(); ok && pw != "" && strings.Contains(masked, pw) {
					t.Errorf("masked output %q leaks raw password %q", masked, pw)
				}
			}
		})
	}
}

// An unparseable proxy value that still contains credentials must be redacted
// entirely, never echoed raw (regression guard for the credential-leak fix).
func TestMaskProxyURL_UnparseableRedacts(t *testing.T) {
	in := "http://user:s3cr3t@%zzproxy.corp:8080" // %zz is an invalid escape → url.Parse errors
	masked, hasCred := maskProxyURL(in)
	if strings.Contains(masked, "s3cr3t") {
		t.Fatalf("masked output %q leaks the raw password", masked)
	}
	if !hasCred {
		t.Errorf("hasCred = false, want true for a credentialed-but-unparseable proxy URL")
	}
	if masked == in {
		t.Errorf("masked output was returned verbatim (unredacted): %q", masked)
	}
}

// ─── env vars ────────────────────────────────────────────────────────────────

func TestDetectEnvProxyCandidates_LowercasePreferredAndMismatch(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "http://upper.proxy:8080")
	t.Setenv("http_proxy", "http://lower.proxy:8080")
	t.Setenv("https_proxy", "http://only-lower.proxy:8443")
	t.Setenv("NO_PROXY", "localhost")

	candidates, mismatches := detectEnvProxyCandidates()

	if len(mismatches) != 1 || mismatches[0] != "HTTP_PROXY/http_proxy" {
		t.Fatalf("mismatches = %v, want [HTTP_PROXY/http_proxy]", mismatches)
	}

	byCategory := map[string]proxyCandidate{}
	for _, c := range candidates {
		byCategory[c.category] = c
	}
	if got := byCategory["http"].value; got != "http://lower.proxy:8080" {
		t.Errorf("http candidate = %q, want the LOWERCASE value", got)
	}
	if got := byCategory["https"].value; got != "http://only-lower.proxy:8443" {
		t.Errorf("https candidate = %q, want %q", got, "http://only-lower.proxy:8443")
	}
	if got := byCategory["no"].value; got != "localhost" {
		t.Errorf("no candidate = %q, want %q", got, "localhost")
	}
	if _, ok := byCategory["all"]; ok {
		t.Errorf("all_proxy candidate present but ALL_PROXY/all_proxy were never set")
	}
	for _, c := range candidates {
		if c.source != ProxySourceEnv {
			t.Errorf("candidate %+v source = %q, want %q", c, c.source, ProxySourceEnv)
		}
	}
}

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range envProxyKeys {
		t.Setenv(k.upper, "")
		os.Unsetenv(k.upper)
		t.Setenv(k.lower, "")
		os.Unsetenv(k.lower)
	}
}

// ─── shell profiles ──────────────────────────────────────────────────────────

func TestDetectShellProxyCandidates_TempProfileAndCredentialMasking(t *testing.T) {
	home := t.TempDir()
	bashrc := "# some comment\nexport HTTP_PROXY=\"http://user:secretpass@corp.proxy:8080\"\nexport NO_PROXY=localhost,127.0.0.1\n"
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(bashrc), 0o644); err != nil {
		t.Fatal(err)
	}

	candidates := detectShellProxyCandidates(home)

	var http, no *proxyCandidate
	for i := range candidates {
		switch candidates[i].category {
		case "http":
			http = &candidates[i]
		case "no":
			no = &candidates[i]
		}
	}
	if http == nil {
		t.Fatal("no http candidate found from the temp .bashrc")
	}
	if http.source != ProxySourceShell {
		t.Errorf("http candidate source = %q, want %q", http.source, ProxySourceShell)
	}
	if got := http.value; got != "http://user:secretpass@corp.proxy:8080" {
		t.Errorf("raw shell candidate value = %q (masking happens at newProxySetting, not here)", got)
	}
	// The setting built from this candidate must mask the credential.
	setting := newProxySetting(http.value, http.source, http.detail)
	if !setting.HasCredentials {
		t.Error("HasCredentials = false, want true (bashrc value carries user:pass)")
	}
	if want := "http://user:***@corp.proxy:8080"; setting.Value != want {
		t.Errorf("masked setting value = %q, want %q", setting.Value, want)
	}
	if strings.Contains(setting.Value, "secretpass") {
		t.Errorf("masked setting value %q leaks the raw password", setting.Value)
	}
	if no == nil || no.value != "localhost,127.0.0.1" {
		t.Errorf("no candidate = %+v, want value localhost,127.0.0.1", no)
	}
}

func TestDetectShellProxyCandidates_MissingFilesTolerated(t *testing.T) {
	// A home dir with no profile files at all must not error or panic.
	home := t.TempDir()
	if candidates := detectShellProxyCandidates(home); len(candidates) != 0 {
		// It's fine if system-wide /etc/environment or /etc/profile.d happen to
		// declare a proxy on the box running this test; just prove home-relative
		// files being absent didn't blow up. Nothing further to assert here.
		t.Logf("got %d candidate(s) from system-wide files (tolerated): %+v", len(candidates), candidates)
	}
}

// ─── git config ──────────────────────────────────────────────────────────────

func TestDetectGitProxyConfig_FakeExec(t *testing.T) {
	orig := execCommandOutput
	defer func() { execCommandOutput = orig }()

	execCommandOutput = func(name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Fatalf("unexpected command %q", name)
		}
		if len(args) == 4 && args[3] == "http.proxy" {
			return []byte("http://git.proxy:8080\n"), nil
		}
		// https.proxy unset: git exits 1.
		return nil, &exec.ExitError{}
	}

	cfg := detectGitProxyConfig()
	if cfg == nil {
		t.Fatal("detectGitProxyConfig() = nil, want a config with http.proxy set")
	}
	if cfg.HTTPProxy == nil || cfg.HTTPProxy.Value != "http://git.proxy:8080" {
		t.Errorf("HTTPProxy = %+v, want value http://git.proxy:8080", cfg.HTTPProxy)
	}
	if cfg.HTTPSProxy != nil {
		t.Errorf("HTTPSProxy = %+v, want nil (https.proxy was never set)", cfg.HTTPSProxy)
	}
}

func TestDetectGitProxyConfig_NeitherSet(t *testing.T) {
	orig := execCommandOutput
	defer func() { execCommandOutput = orig }()
	execCommandOutput = func(name string, args ...string) ([]byte, error) {
		return nil, &exec.ExitError{}
	}
	if cfg := detectGitProxyConfig(); cfg != nil {
		t.Errorf("detectGitProxyConfig() = %+v, want nil", cfg)
	}
}

// ─── tool configs ────────────────────────────────────────────────────────────

func TestDetectToolConfigs_TempHome(t *testing.T) {
	home := t.TempDir()

	mustWrite(t, filepath.Join(home, ".npmrc"), "registry=https://registry.npmjs.org/\nhttps-proxy=http://npm.proxy:8080\n")
	mustWrite(t, filepath.Join(home, ".config", "pip", "pip.conf"), "[global]\nindex-url = https://pypi.org/simple\nproxy = http://pip.proxy:8080\n")
	mustWrite(t, filepath.Join(home, ".cargo", "config.toml"), "[http]\nproxy = \"http://cargo.proxy:8080\"\n[source.crates-io]\nreplace-with = \"corp\"\n")
	mustWrite(t, filepath.Join(home, ".m2", "settings.xml"),
		"<settings><proxies><proxy><id>corp</id><active>true</active><host>maven.proxy</host><port>8080</port></proxy></proxies></settings>")

	// apt: point the package var at a temp dir instead of the real /etc.
	origApt := aptConfDir
	defer func() { aptConfDir = origApt }()
	aptConfDir = t.TempDir()
	mustWrite(t, filepath.Join(aptConfDir, "01proxy"), `Acquire::http::Proxy "http://apt.proxy:8080";`+"\n")

	got := detectToolConfigs(home)
	byTool := map[string]HostProxyToolConfig{}
	for _, tc := range got {
		byTool[tc.Tool] = tc
	}

	want := map[string]string{
		"npm":   "http://npm.proxy:8080",
		"pip":   "http://pip.proxy:8080",
		"cargo": "http://cargo.proxy:8080",
		"maven": "maven.proxy:8080",
		"apt":   "http://apt.proxy:8080",
	}
	for tool, wantVal := range want {
		tc, ok := byTool[tool]
		if !ok {
			t.Errorf("no %s tool config detected; got %+v", tool, got)
			continue
		}
		if tc.Setting.Value != wantVal {
			t.Errorf("%s setting = %q, want %q", tool, tc.Setting.Value, wantVal)
		}
		if tc.Setting.Source != ProxySourceTool {
			t.Errorf("%s source = %q, want %q", tool, tc.Setting.Source, ProxySourceTool)
		}
	}
	if len(got) != len(want) {
		t.Errorf("detectToolConfigs returned %d entries, want %d: %+v", len(got), len(want), got)
	}
}

func TestDetectToolConfigs_AbsentFilesProduceNoEntries(t *testing.T) {
	home := t.TempDir() // empty — none of npm/pip/cargo/maven configs exist
	origApt := aptConfDir
	defer func() { aptConfDir = origApt }()
	aptConfDir = t.TempDir() // empty dir too

	if got := detectToolConfigs(home); len(got) != 0 {
		t.Errorf("detectToolConfigs on an empty home = %+v, want no entries", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ─── OS-level parsing (pure — no real exec, safe on any box) ────────────────

func TestSplitWindowsProxyServer(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  map[string]string
	}{
		{"bare", "corp.proxy:8080", map[string]string{"http": "corp.proxy:8080", "https": "corp.proxy:8080"}},
		{"scheme_split", "http=corp.proxy:8080;https=corp.proxy:8443", map[string]string{"http": "corp.proxy:8080", "https": "corp.proxy:8443"}},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitWindowsProxyServer(tc.value, "detail")
			gotMap := map[string]string{}
			for _, c := range got {
				gotMap[c.category] = c.value
			}
			if len(gotMap) != len(tc.want) {
				t.Fatalf("splitWindowsProxyServer(%q) = %+v, want %+v", tc.value, gotMap, tc.want)
			}
			for k, v := range tc.want {
				if gotMap[k] != v {
					t.Errorf("category %q = %q, want %q", k, gotMap[k], v)
				}
			}
		})
	}
}

func TestParseWindowsRegistryProxyJSON(t *testing.T) {
	raw := []byte(`{"ProxyServer":"http=corp.proxy:8080;https=corp.proxy:8443","ProxyEnable":1,"AutoConfigURL":"http://wpad.corp/proxy.pac"}`)
	candidates, pac := parseWindowsRegistryProxyJSON(raw)
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v, want 2 entries", candidates)
	}
	if pac == nil || pac.URL != "http://wpad.corp/proxy.pac" {
		t.Fatalf("pac = %+v, want the wpad URL", pac)
	}

	// ProxyEnable=0 must suppress the proxy candidates (but PAC, if present in
	// this fixture, is independent of ProxyEnable).
	raw2 := []byte(`{"ProxyServer":"corp.proxy:8080","ProxyEnable":0,"AutoConfigURL":""}`)
	candidates2, pac2 := parseWindowsRegistryProxyJSON(raw2)
	if len(candidates2) != 0 {
		t.Errorf("candidates2 = %+v, want none (ProxyEnable=0)", candidates2)
	}
	if pac2 != nil {
		t.Errorf("pac2 = %+v, want nil", pac2)
	}

	// Malformed JSON must not panic or error out.
	if c, p := parseWindowsRegistryProxyJSON([]byte("not json")); c != nil || p != nil {
		t.Errorf("malformed JSON produced %+v / %+v, want nil/nil", c, p)
	}
}

func TestParseNetshWinHTTPOutput(t *testing.T) {
	direct := "Current WinHTTP proxy settings:\n\n    Direct access (no proxy server).\n"
	if got := parseNetshWinHTTPOutput(direct); got != nil {
		t.Errorf("direct-access output produced %+v, want nil", got)
	}

	proxied := "Current WinHTTP proxy settings:\n\n    Proxy Server(s) :  corp.proxy:8080\n    Bypass List     :  (none)\n"
	got := parseNetshWinHTTPOutput(proxied)
	if len(got) != 2 {
		t.Fatalf("got = %+v, want 2 candidates (http+https)", got)
	}
}

func TestParseScutilProxyOutput(t *testing.T) {
	sample := `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
  }
  HTTPEnable : 1
  HTTPPort : 8080
  HTTPProxy : proxy.corp.com
  HTTPSEnable : 1
  HTTPSPort : 8443
  HTTPSProxy : proxy.corp.com
  ProxyAutoConfigEnable : 1
  ProxyAutoConfigURLString : http://wpad.corp.com/proxy.pac
}
`
	candidates, pac := parseScutilProxyOutput(sample)
	byCategory := map[string]string{}
	for _, c := range candidates {
		byCategory[c.category] = c.value
	}
	if byCategory["http"] != "proxy.corp.com:8080" {
		t.Errorf("http = %q, want proxy.corp.com:8080", byCategory["http"])
	}
	if byCategory["https"] != "proxy.corp.com:8443" {
		t.Errorf("https = %q, want proxy.corp.com:8443", byCategory["https"])
	}
	if pac == nil || pac.URL != "http://wpad.corp.com/proxy.pac" {
		t.Fatalf("pac = %+v, want the wpad URL", pac)
	}
}

// ─── OS-level: Windows via WSL (fake exec) ──────────────────────────────────

// TestDetectWindowsProxyViaWSL_RegistryHit exercises the primary probe: a
// successful powershell.exe call reporting an enabled proxy must produce
// candidates and must NOT fall back to netsh.exe (netsh is only consulted
// when the registry probe yields nothing).
func TestDetectWindowsProxyViaWSL_RegistryHit(t *testing.T) {
	orig := execCommandOutput
	defer func() { execCommandOutput = orig }()

	execCommandOutput = func(name string, args ...string) ([]byte, error) {
		if name != "powershell.exe" {
			t.Fatalf("unexpected command %q; netsh.exe must not run once the registry probe found a proxy", name)
		}
		return []byte(`{"ProxyServer":"http=corp.proxy:8080;https=corp.proxy:8443","ProxyEnable":1,"AutoConfigURL":""}`), nil
	}

	candidates, pac := detectWindowsProxyViaWSL()
	byCategory := map[string]string{}
	for _, c := range candidates {
		byCategory[c.category] = c.value
	}
	if byCategory["http"] != "corp.proxy:8080" || byCategory["https"] != "corp.proxy:8443" {
		t.Fatalf("candidates = %+v, want http=corp.proxy:8080 https=corp.proxy:8443", candidates)
	}
	if pac != nil {
		t.Errorf("pac = %+v, want nil (AutoConfigURL empty)", pac)
	}
}

// TestDetectWindowsProxyViaWSL_FallsBackToNetsh exercises the secondary
// probe: when the registry probe yields no proxy, netsh.exe winhttp is
// consulted next.
func TestDetectWindowsProxyViaWSL_FallsBackToNetsh(t *testing.T) {
	orig := execCommandOutput
	defer func() { execCommandOutput = orig }()

	var calls []string
	execCommandOutput = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name)
		switch name {
		case "powershell.exe":
			return []byte(`{"ProxyServer":"","ProxyEnable":0,"AutoConfigURL":""}`), nil
		case "netsh.exe":
			return []byte("Current WinHTTP proxy settings:\n\n    Proxy Server(s) :  corp.proxy:8080\n"), nil
		default:
			t.Fatalf("unexpected command %q", name)
			return nil, nil
		}
	}

	candidates, pac := detectWindowsProxyViaWSL()
	byCategory := map[string]string{}
	for _, c := range candidates {
		byCategory[c.category] = c.value
	}
	if byCategory["http"] != "corp.proxy:8080" || byCategory["https"] != "corp.proxy:8080" {
		t.Fatalf("candidates = %+v, want the netsh fallback value on both categories", candidates)
	}
	if pac != nil {
		t.Errorf("pac = %+v, want nil", pac)
	}
	if len(calls) != 2 || calls[0] != "powershell.exe" || calls[1] != "netsh.exe" {
		t.Errorf("calls = %v, want [powershell.exe netsh.exe] in that order", calls)
	}
}

// TestDetectWindowsProxyViaWSL_BothUnavailable proves the dispatch tolerates
// powershell.exe/netsh.exe both being absent (e.g. interop off, or not
// actually running under WSL).
func TestDetectWindowsProxyViaWSL_BothUnavailable(t *testing.T) {
	orig := execCommandOutput
	defer func() { execCommandOutput = orig }()
	execCommandOutput = func(name string, args ...string) ([]byte, error) {
		return nil, &exec.ExitError{}
	}

	candidates, pac := detectWindowsProxyViaWSL()
	if candidates != nil || pac != nil {
		t.Errorf("candidates=%+v pac=%+v, want nil/nil when both probes are unavailable", candidates, pac)
	}
}

// ─── OS-level: macOS (fake exec) ─────────────────────────────────────────────

// TestDetectMacOSProxy_FakeExec exercises the scutil dispatch+parse wiring
// end to end via the execCommandOutput seam (mirroring
// TestDetectGitProxyConfig_FakeExec's pattern for the git probe).
func TestDetectMacOSProxy_FakeExec(t *testing.T) {
	orig := execCommandOutput
	defer func() { execCommandOutput = orig }()

	execCommandOutput = func(name string, args ...string) ([]byte, error) {
		if name != "scutil" || len(args) != 1 || args[0] != "--proxy" {
			t.Fatalf("unexpected command %q %v, want `scutil --proxy`", name, args)
		}
		return []byte("<dictionary> {\n  HTTPEnable : 1\n  HTTPPort : 8080\n  HTTPProxy : proxy.corp.com\n}\n"), nil
	}

	candidates, pac := detectMacOSProxy()
	if len(candidates) != 1 || candidates[0].category != "http" || candidates[0].value != "proxy.corp.com:8080" {
		t.Fatalf("candidates = %+v, want one http candidate proxy.corp.com:8080", candidates)
	}
	if pac != nil {
		t.Errorf("pac = %+v, want nil", pac)
	}
}

// TestDetectMacOSProxy_Unavailable proves the dispatch tolerates scutil being
// absent (e.g. not actually running on macOS).
func TestDetectMacOSProxy_Unavailable(t *testing.T) {
	orig := execCommandOutput
	defer func() { execCommandOutput = orig }()
	execCommandOutput = func(name string, args ...string) ([]byte, error) {
		return nil, &exec.ExitError{}
	}

	candidates, pac := detectMacOSProxy()
	if candidates != nil || pac != nil {
		t.Errorf("candidates=%+v pac=%+v, want nil/nil when scutil is unavailable", candidates, pac)
	}
}

// ─── precedence merge ────────────────────────────────────────────────────────

func TestWinningSetting_PrecedenceEnvBeatsShellBeatsOS(t *testing.T) {
	candidates := []proxyCandidate{
		{"http", "http://env.proxy:8080", ProxySourceEnv, "env"},
		{"http", "http://shell.proxy:8080", ProxySourceShell, "shell"},
		{"http", "http://os.proxy:8080", ProxySourceOS, "os"},
		{"https", "http://shell.proxy:8443", ProxySourceShell, "shell"},
		{"https", "http://os.proxy:8443", ProxySourceOS, "os"},
		{"all", "http://os.proxy:9000", ProxySourceOS, "os"},
	}
	if got := winningSetting(candidates, "http"); got == nil || got.Value != "http://env.proxy:8080" || got.Source != ProxySourceEnv {
		t.Errorf("http winner = %+v, want the env candidate", got)
	}
	if got := winningSetting(candidates, "https"); got == nil || got.Value != "http://shell.proxy:8443" || got.Source != ProxySourceShell {
		t.Errorf("https winner = %+v, want the shell candidate (no env set)", got)
	}
	if got := winningSetting(candidates, "all"); got == nil || got.Source != ProxySourceOS {
		t.Errorf("all winner = %+v, want the OS candidate (no env/shell set)", got)
	}
	if got := winningSetting(candidates, "no"); got != nil {
		t.Errorf("no winner = %+v, want nil (nothing set)", got)
	}
}

// ─── top-level smoke test ────────────────────────────────────────────────────

// TestDetectHostProxy_NeverPanicsOrErrors is a smoke test: DetectHostProxy
// must return successfully on any box, including one where powershell.exe,
// netsh.exe, scutil, or git are all absent (every detector tolerates that).
func TestDetectHostProxy_NeverPanicsOrErrors(t *testing.T) {
	clearProxyEnv(t)
	got := DetectHostProxy()
	if detectionHasCredentials(got) && !got.HasCredentials {
		t.Error("HasCredentials inconsistent with the per-field flags")
	}
}
