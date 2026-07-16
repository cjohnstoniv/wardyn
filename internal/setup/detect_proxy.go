// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package setup

// detect_proxy.go — host proxy DETECTION (read-only, best-effort). This is the
// detection half of the Getting-Started "Host Proxy" step (see the R4 catalog
// in the setup-readiness plan): find the common host-side proxy mechanisms so
// the wizard can surface + suggest settings. It does NOT configure anything —
// the upstream-proxy plumbing (wardyn-proxy dialing a corp proxy) is separate.
//
// Same leaf-package constraint as detect.go: stdlib only.
//
// Precedence (per the R4 catalog): env > shell profile > OS. Git config and
// per-tool configs (npm/pip/cargo/maven/apt) are surfaced as their OWN fields
// rather than folded into that merge — git resolves its own proxy independent
// of env ("git's own wins over env"), and each tool's config is a standalone
// override for that tool only, not a generic system signal. PAC/WPAD is
// FLAG-only: it is arbitrary JS, so it is never fetched or evaluated.
//
// Credential safety: any detected proxy URL containing userinfo (user[:pass]@)
// is masked to "user:***@host" (or "user@host" with no password) before it is
// ever stored in the returned struct — the raw credential is never retained,
// returned, or logged. HasCredentials flags that a credential WAS present, so
// the UI can offer to store it as a secret instead of a plain value.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Bounds on file reads for the detectors below — a hostile/huge file must
// never make detection slow or exhaust memory (best-effort scan, not a full
// parse).
const (
	maxShellProfileLines = 2000
	maxToolConfigLines   = 2000
	maxToolConfigBytes   = 1 << 20 // 1 MiB — bounds the maven settings.xml whole-file read
)

// aptConfDir is where apt proxy config fragments live. A package var so tests
// can point it at a temp dir instead of the real /etc/apt/apt.conf.d.
var aptConfDir = "/etc/apt/apt.conf.d"

// probeTimeout bounds each interop probe. DetectHostProxy runs synchronously on
// every GET /setup/status, so a wedged WSL powershell.exe / netsh / scutil must
// never hang the wizard poll — it fails fast to "not detected" instead.
const probeTimeout = 3 * time.Second

// execCommandOutput runs an external command and returns its stdout. Used for
// every interop probe below (git, powershell.exe, netsh.exe, scutil) — a
// package var so tests can fake responses without touching the real host. The
// call is bounded by probeTimeout (a hung host tool yields an error, not a hang).
var execCommandOutput = func(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// HostProxySource is where a detected proxy setting came from.
type HostProxySource string

const (
	ProxySourceEnv   HostProxySource = "env"
	ProxySourceShell HostProxySource = "shell_profile"
	ProxySourceGit   HostProxySource = "git_config"
	ProxySourceTool  HostProxySource = "tool_config"
	ProxySourceOS    HostProxySource = "os"
)

// HostProxySetting is one detected proxy value, safe to render directly: any
// embedded credential has already been masked (see maskProxyURL) — the raw
// value is never retained. HasCredentials flags that a credential WAS present
// in the raw value, so the UI can prompt to store it as a secret.
type HostProxySetting struct {
	Value          string          `json:"value"`
	Source         HostProxySource `json:"source"`
	Detail         string          `json:"detail,omitempty"`
	HasCredentials bool            `json:"has_credentials"`
}

// HostProxyGitConfig is `git config --global http.proxy` / `https.proxy` —
// kept separate from the generic HTTP(S)_PROXY resolution because git honors
// its own config over the environment.
type HostProxyGitConfig struct {
	HTTPProxy  *HostProxySetting `json:"http_proxy,omitempty"`
	HTTPSProxy *HostProxySetting `json:"https_proxy,omitempty"`
}

// HostProxyToolConfig is one per-tool proxy directive found in a config file
// that does NOT honor HTTP_PROXY/HTTPS_PROXY env vars (npm/pip/cargo honor env
// already and are skipped; this covers git-adjacent tools that need explicit
// config: maven, cargo's own config file, apt). Informational only — nothing
// here is ever written to.
type HostProxyToolConfig struct {
	Tool    string           `json:"tool"`
	Path    string           `json:"path"`
	Setting HostProxySetting `json:"setting"`
}

// HostProxyPAC flags a discovered PAC/WPAD auto-config URL. It is never
// fetched or executed (it's arbitrary JS) — presence alone is surfaced so the
// operator can resolve the effective proxy manually.
type HostProxyPAC struct {
	URL    string          `json:"url"`
	Source HostProxySource `json:"source"`
	Detail string          `json:"detail,omitempty"`
}

// HostProxyDetection is the aggregate, masked-safe host-proxy detection
// result. Every field is best-effort: absence of a mechanism (no env var, no
// git config, powershell.exe unavailable, ...) is not an error.
type HostProxyDetection struct {
	HTTPProxy  *HostProxySetting `json:"http_proxy,omitempty"`
	HTTPSProxy *HostProxySetting `json:"https_proxy,omitempty"`
	AllProxy   *HostProxySetting `json:"all_proxy,omitempty"`
	NoProxy    *HostProxySetting `json:"no_proxy,omitempty"`

	// EnvCaseMismatch lists "UPPER/lower" pairs whose values disagree (httpoxy
	// hygiene warning) — the lowercase value is still what wins the merge.
	EnvCaseMismatch []string `json:"env_case_mismatch,omitempty"`

	GitProxy *HostProxyGitConfig `json:"git_proxy,omitempty"`

	ToolConfigs []HostProxyToolConfig `json:"tool_configs,omitempty"`

	PAC *HostProxyPAC `json:"pac,omitempty"`

	// HasCredentials is true when ANY of the above carried a masked
	// credential — a single convenience flag the UI can check without
	// walking every sub-field.
	HasCredentials bool `json:"has_credentials"`
}

// DetectHostProxy runs every detector in the R4 catalog and merges the
// generic HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY signal by precedence
// (env > shell profile > OS). It never fails: every detector tolerates the
// absence of its mechanism (missing file, missing binary, non-zero exit).
func DetectHostProxy() HostProxyDetection {
	home, _ := os.UserHomeDir()

	envCandidates, mismatches := detectEnvProxyCandidates()
	shellCandidates := detectShellProxyCandidates(home)
	osCandidates, pac := detectOSProxyCandidates()

	merged := make([]proxyCandidate, 0, len(envCandidates)+len(shellCandidates)+len(osCandidates))
	merged = append(merged, envCandidates...)
	merged = append(merged, shellCandidates...)
	merged = append(merged, osCandidates...)

	det := HostProxyDetection{
		EnvCaseMismatch: mismatches,
		GitProxy:        detectGitProxyConfig(),
		ToolConfigs:     detectToolConfigs(home),
		PAC:             pac,
		HTTPProxy:       winningSetting(merged, "http"),
		HTTPSProxy:      winningSetting(merged, "https"),
		AllProxy:        winningSetting(merged, "all"),
		NoProxy:         winningSetting(merged, "no"),
	}
	det.HasCredentials = detectionHasCredentials(det)
	return det
}

// proxyCandidate is one detected value competing to win its category ("http",
// "https", "all", "no") in the env>shell>OS merge.
type proxyCandidate struct {
	category string
	value    string
	source   HostProxySource
	detail   string
}

// winningSetting returns the first (highest-precedence) candidate for
// category, masked into a HostProxySetting, or nil if none was found.
// Candidates must already be in precedence order (env, then shell, then OS).
func winningSetting(candidates []proxyCandidate, category string) *HostProxySetting {
	for _, c := range candidates {
		if c.category == category {
			return newProxySetting(c.value, c.source, c.detail)
		}
	}
	return nil
}

// newProxySetting builds a masked-safe HostProxySetting from a raw value.
func newProxySetting(raw string, source HostProxySource, detail string) *HostProxySetting {
	masked, hasCred := maskProxyURL(raw)
	return &HostProxySetting{Value: masked, Source: source, Detail: detail, HasCredentials: hasCred}
}

// detectionHasCredentials reports whether any field in det carries a masked
// credential.
func detectionHasCredentials(det HostProxyDetection) bool {
	for _, s := range []*HostProxySetting{det.HTTPProxy, det.HTTPSProxy, det.AllProxy, det.NoProxy} {
		if s != nil && s.HasCredentials {
			return true
		}
	}
	if det.GitProxy != nil {
		if det.GitProxy.HTTPProxy != nil && det.GitProxy.HTTPProxy.HasCredentials {
			return true
		}
		if det.GitProxy.HTTPSProxy != nil && det.GitProxy.HTTPSProxy.HasCredentials {
			return true
		}
	}
	for _, tc := range det.ToolConfigs {
		if tc.Setting.HasCredentials {
			return true
		}
	}
	return false
}

// maskProxyURL parses raw as a proxy address and, when it carries userinfo
// (user[:pass]@host), returns a display-safe form with any password replaced
// by "***" — the raw credential is never returned. Values with no userinfo
// (including a bare "host:port", and anything net/url can't parse) are
// returned UNCHANGED with hasCredentials=false: there is nothing to mask.
func maskProxyURL(raw string) (masked string, hasCredentials bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	hadScheme := strings.Contains(raw, "://")
	parseTarget := raw
	if !hadScheme {
		// Bare "host:port" has no place for net/url to find userinfo unless it
		// looks like a URL; adding a scheme lets url.Parse do the real work.
		parseTarget = "http://" + raw
	}
	u, err := url.Parse(parseTarget)
	if err != nil {
		// Parse failed → we cannot locate embedded userinfo to redact. If the value
		// could carry credentials (contains '@'), redact it entirely rather than echo
		// a possibly-credentialed URL. Previously this returned the raw string here,
		// leaking credentials on any proxy value net/url rejected.
		if strings.Contains(raw, "@") {
			return "<redacted: unparseable proxy URL>", true
		}
		return raw, false
	}
	if u.User == nil || u.User.Username() == "" {
		return raw, false
	}
	display := u.User.Username()
	if _, hasPass := u.User.Password(); hasPass {
		display += ":***"
	}
	rest := u.Host + u.Path
	if u.RawQuery != "" {
		rest += "?" + u.RawQuery
	}
	if hadScheme {
		return u.Scheme + "://" + display + "@" + rest, true
	}
	return display + "@" + rest, true
}

// ─── env vars ────────────────────────────────────────────────────────────────

// envProxyKeys pairs each category with its upper/lower env var names.
var envProxyKeys = []struct{ category, upper, lower string }{
	{"http", "HTTP_PROXY", "http_proxy"},
	{"https", "HTTPS_PROXY", "https_proxy"},
	{"all", "ALL_PROXY", "all_proxy"},
	{"no", "NO_PROXY", "no_proxy"},
}

// detectEnvProxyCandidates reads the env-var mechanism: lowercase preferred
// (httpoxy hygiene) when both cases are set; mismatches records any pair whose
// values disagree so the step can warn about it.
func detectEnvProxyCandidates() (candidates []proxyCandidate, mismatches []string) {
	for _, k := range envProxyKeys {
		upperVal, upperOK := os.LookupEnv(k.upper)
		lowerVal, lowerOK := os.LookupEnv(k.lower)
		switch {
		case lowerOK && upperOK:
			if upperVal != lowerVal {
				mismatches = append(mismatches, k.upper+"/"+k.lower)
			}
			candidates = append(candidates, proxyCandidate{k.category, lowerVal, ProxySourceEnv,
				k.lower + " (lowercase preferred; " + k.upper + " also set and disagrees is flagged)"})
		case lowerOK:
			candidates = append(candidates, proxyCandidate{k.category, lowerVal, ProxySourceEnv, k.lower})
		case upperOK:
			candidates = append(candidates, proxyCandidate{k.category, upperVal, ProxySourceEnv,
				k.upper + " (no lowercase variant set)"})
		}
	}
	return candidates, mismatches
}

// ─── shell profiles ──────────────────────────────────────────────────────────

// shellProxyLineRE captures a `[export ]FOO_PROXY=value` declaration. Group 1
// is the variable name (case preserved for display), group 2 the value
// (quotes stripped). This is a STATIC read of the file — it does not confirm
// the variable is actually exported in a live shell.
var shellProxyLineRE = regexp.MustCompile(`(?i)^\s*(?:export\s+)?(https?_proxy|all_proxy|no_proxy)\s*=\s*['"]?([^'"\s]+)['"]?`)

// shellProfilePaths returns the fixed set of shell-profile locations to grep,
// home-relative files first (so a per-user override wins over a system-wide
// one when both declare the same variable — see detectShellProxyCandidates'
// first-match-wins dedup).
func shellProfilePaths(home string) []string {
	var paths []string
	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".bashrc"),
			filepath.Join(home, ".bash_profile"),
			filepath.Join(home, ".profile"),
			filepath.Join(home, ".zshrc"),
		)
	}
	paths = append(paths, "/etc/environment")
	if matches, err := filepath.Glob("/etc/profile.d/*.sh"); err == nil {
		sort.Strings(matches)
		paths = append(paths, matches...)
	}
	return paths
}

// proxyEnvKeyCategory maps a *_PROXY variable name (any case) to its category.
func proxyEnvKeyCategory(key string) string {
	switch strings.ToLower(key) {
	case "http_proxy":
		return "http"
	case "https_proxy":
		return "https"
	case "all_proxy":
		return "all"
	case "no_proxy":
		return "no"
	}
	return ""
}

// detectShellProxyCandidates greps the shell-profile mechanism. Absent files
// are silently skipped (best-effort). First file to declare a category wins
// (shellProfilePaths orders home files before system-wide ones).
func detectShellProxyCandidates(home string) []proxyCandidate {
	var out []proxyCandidate
	seen := map[string]bool{}
	for _, path := range shellProfilePaths(home) {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for lines := 0; scanner.Scan() && lines < maxShellProfileLines; lines++ {
			m := shellProxyLineRE.FindStringSubmatch(scanner.Text())
			if m == nil {
				continue
			}
			category := proxyEnvKeyCategory(m[1])
			if category == "" || seen[category] {
				continue
			}
			seen[category] = true
			out = append(out, proxyCandidate{category, m[2], ProxySourceShell, m[1] + " declared in " + path})
		}
		f.Close()
	}
	return out
}

// ─── git config ──────────────────────────────────────────────────────────────

// gitConfigGet runs `git config --global --get <key>`, tolerating git's
// absence and an unset key (both surface as a non-nil error / exit 1).
func gitConfigGet(key string) (string, bool) {
	out, err := execCommandOutput("git", "config", "--global", "--get", key)
	if err != nil {
		return "", false
	}
	val := strings.TrimSpace(string(out))
	if val == "" {
		return "", false
	}
	return val, true
}

// detectGitProxyConfig reads `git config --global http.proxy` / `https.proxy`.
// Returns nil when neither is set (git absent or unconfigured).
func detectGitProxyConfig() *HostProxyGitConfig {
	httpVal, httpOK := gitConfigGet("http.proxy")
	httpsVal, httpsOK := gitConfigGet("https.proxy")
	if !httpOK && !httpsOK {
		return nil
	}
	cfg := &HostProxyGitConfig{}
	if httpOK {
		cfg.HTTPProxy = newProxySetting(httpVal, ProxySourceGit, "git config --global http.proxy")
	}
	if httpsOK {
		cfg.HTTPSProxy = newProxySetting(httpsVal, ProxySourceGit, "git config --global https.proxy")
	}
	return cfg
}

// ─── tool configs (informational) ───────────────────────────────────────────

// readFileCapped reads up to max bytes of path (bounds a whole-file read like
// maven's settings.xml, which needs cross-line matching rather than a
// line scan).
func readFileCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// grepFirstMatch returns the first regex group-1 match found in path (line by
// line, bounded), or ok=false if the file is absent or no line matches.
func grepFirstMatch(path string, re *regexp.Regexp) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for lines := 0; scanner.Scan() && lines < maxToolConfigLines; lines++ {
		if m := re.FindStringSubmatch(scanner.Text()); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// grepIniSection is grepFirstMatch scoped to lines inside an INI/TOML
// `[section]` header (case-insensitive), for formats where the same key name
// is only meaningful under a specific section (pip.conf [global], cargo
// config.toml [http]).
func grepIniSection(path, section string, re *regexp.Regexp) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	current := ""
	for lines := 0; scanner.Scan() && lines < maxToolConfigLines; lines++ {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		if !strings.EqualFold(current, section) {
			continue
		}
		if m := re.FindStringSubmatch(line); m != nil {
			return m[1], true
		}
	}
	return "", false
}

var (
	npmProxyRE    = regexp.MustCompile(`(?i)^(?:https-proxy|proxy)\s*=\s*(\S+)`)
	pipProxyRE    = regexp.MustCompile(`^proxy\s*=\s*(\S+)`)
	cargoProxyRE  = regexp.MustCompile(`^proxy\s*=\s*"?([^"\s]+)"?`)
	aptProxyRE    = regexp.MustCompile(`Acquire::(?:http|https)::Proxy\s+"([^"]+)"`)
	mavenProxyRE  = regexp.MustCompile(`(?s)<proxies>(.*?)</proxies>`)
	mavenHostRE   = regexp.MustCompile(`<host>\s*([^<\s]+)\s*</host>`)
	mavenPortRE   = regexp.MustCompile(`<port>\s*([^<\s]+)\s*</port>`)
	mavenActiveRE = regexp.MustCompile(`<active>\s*false\s*</active>`)
)

// detectMavenProxy reads ~/.m2/settings.xml for a <proxies><proxy> block.
// A light regex scan, not a real XML parse (informational only; good enough
// to surface host:port and whether the entry is explicitly inactive).
func detectMavenProxy(path string) (string, bool) {
	data, err := readFileCapped(path, maxToolConfigBytes)
	if err != nil {
		return "", false
	}
	m := mavenProxyRE.FindSubmatch(data)
	if m == nil {
		return "", false
	}
	block := m[1]
	host := mavenHostRE.FindSubmatch(block)
	if host == nil {
		return "", false
	}
	val := string(host[1])
	if port := mavenPortRE.FindSubmatch(block); port != nil {
		val += ":" + string(port[1])
	}
	if mavenActiveRE.Match(block) {
		val += " (inactive: <active>false</active>)"
	}
	return val, true
}

// detectAptProxy scans every file under aptConfDir for an
// Acquire::http[s]::Proxy directive.
func detectAptProxy() (value, path string, ok bool) {
	matches, err := filepath.Glob(filepath.Join(aptConfDir, "*"))
	if err != nil {
		return "", "", false
	}
	sort.Strings(matches)
	for _, p := range matches {
		if v, found := grepFirstMatch(p, aptProxyRE); found {
			return v, p, true
		}
	}
	return "", "", false
}

// toolConfig wraps a raw detected value into a masked HostProxyToolConfig.
func toolConfig(tool, path, raw string) HostProxyToolConfig {
	return HostProxyToolConfig{Tool: tool, Path: path, Setting: *newProxySetting(raw, ProxySourceTool, tool+" config ("+path+")")}
}

// detectToolConfigs covers the non-env-honoring tool configs from the R4
// catalog: npm/pip/cargo have their own config-file proxy directive (in
// addition to already honoring HTTP_PROXY/HTTPS_PROXY — this surfaces a
// possibly-conflicting override), maven and apt need one (they do not read
// the env at all). Entries are only added when a proxy directive is actually
// found — an absent/unconfigured file produces no entry.
func detectToolConfigs(home string) []HostProxyToolConfig {
	var out []HostProxyToolConfig
	if home != "" {
		if v, ok := grepFirstMatch(filepath.Join(home, ".npmrc"), npmProxyRE); ok {
			out = append(out, toolConfig("npm", filepath.Join(home, ".npmrc"), v))
		}
		pipPath := filepath.Join(home, ".config", "pip", "pip.conf")
		if v, ok := grepIniSection(pipPath, "global", pipProxyRE); ok {
			out = append(out, toolConfig("pip", pipPath, v))
		}
		cargoPath := filepath.Join(home, ".cargo", "config.toml")
		if v, ok := grepIniSection(cargoPath, "http", cargoProxyRE); ok {
			out = append(out, toolConfig("cargo", cargoPath, v))
		}
		mavenPath := filepath.Join(home, ".m2", "settings.xml")
		if v, ok := detectMavenProxy(mavenPath); ok {
			out = append(out, toolConfig("maven", mavenPath, v))
		}
	}
	if v, path, ok := detectAptProxy(); ok {
		out = append(out, toolConfig("apt", path, v))
	}
	return out
}

// ─── OS-level (WSL2 -> Windows, macOS) ──────────────────────────────────────

// detectOSProxyCandidates dispatches to the host's OS-level proxy mechanism:
// Windows Internet Settings / WinHTTP via powershell.exe/netsh.exe interop
// when running under WSL2, or scutil on macOS. Any other host (plain Linux)
// has no OS-level mechanism in the R4 catalog and returns nothing.
func detectOSProxyCandidates() ([]proxyCandidate, *HostProxyPAC) {
	switch {
	case detectWSL():
		return detectWindowsProxyViaWSL()
	case runtime.GOOS == "darwin":
		return detectMacOSProxy()
	default:
		return nil, nil
	}
}

// winRegistryProxy mirrors the HKCU Internet Settings properties read via
// powershell.exe. ProxyEnable is a registry DWORD (0/1).
type winRegistryProxy struct {
	ProxyEnable   int    `json:"ProxyEnable"`
	ProxyServer   string `json:"ProxyServer"`
	AutoConfigURL string `json:"AutoConfigURL"`
}

var netshProxyLineRE = regexp.MustCompile(`(?i)Proxy Server\(s\)\s*:\s*(.*)`)

// detectWindowsProxyViaWSL probes the Windows host from inside WSL2:
// first the per-user Internet Settings registry key (proxy + PAC URL), then
// (only if that yielded nothing) the machine-wide WinHTTP proxy via
// `netsh winhttp show proxy` — a separate, distinct proxy store some
// enterprise agents read instead of Internet Settings. Both calls tolerate
// powershell.exe/netsh.exe being unavailable (interop off, or not WSL at all).
func detectWindowsProxyViaWSL() ([]proxyCandidate, *HostProxyPAC) {
	var candidates []proxyCandidate
	var pac *HostProxyPAC

	if out, err := execCommandOutput("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		`Get-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings' | `+
			`Select-Object ProxyServer,ProxyEnable,AutoConfigURL | ConvertTo-Json -Compress`); err == nil {
		candidates, pac = parseWindowsRegistryProxyJSON(out)
	}

	if pac == nil && len(candidates) == 0 {
		if out, err := execCommandOutput("netsh.exe", "winhttp", "show", "proxy"); err == nil {
			candidates = parseNetshWinHTTPOutput(string(out))
		}
	}
	return candidates, pac
}

// parseWindowsRegistryProxyJSON parses the `ConvertTo-Json` output of the
// Internet Settings probe above. Malformed/empty JSON yields nothing (never
// an error — this is best-effort).
func parseWindowsRegistryProxyJSON(raw []byte) ([]proxyCandidate, *HostProxyPAC) {
	var reg winRegistryProxy
	if err := json.Unmarshal(bytes.TrimSpace(raw), &reg); err != nil {
		return nil, nil
	}
	var candidates []proxyCandidate
	if reg.ProxyEnable != 0 && reg.ProxyServer != "" {
		candidates = splitWindowsProxyServer(reg.ProxyServer, "Windows Internet Settings (HKCU, via powershell.exe)")
	}
	var pac *HostProxyPAC
	if reg.AutoConfigURL != "" {
		pac = &HostProxyPAC{URL: reg.AutoConfigURL, Source: ProxySourceOS,
			Detail: "Windows Internet Settings AutoConfigURL (via powershell.exe)"}
	}
	return candidates, pac
}

// parseNetshWinHTTPOutput parses `netsh winhttp show proxy` text output.
func parseNetshWinHTTPOutput(text string) []proxyCandidate {
	m := netshProxyLineRE.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	val := strings.TrimSpace(m[1])
	if val == "" || strings.Contains(strings.ToLower(val), "direct access") {
		return nil
	}
	return splitWindowsProxyServer(val, "netsh winhttp show proxy")
}

// splitWindowsProxyServer parses a Windows ProxyServer value, which is either
// a bare "host:port" (applies to both http and https) or a scheme-split list
// like "http=proxy:8080;https=proxy:8443".
func splitWindowsProxyServer(value, detail string) []proxyCandidate {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !strings.Contains(value, "=") {
		return []proxyCandidate{
			{"http", value, ProxySourceOS, detail},
			{"https", value, ProxySourceOS, detail},
		}
	}
	var out []proxyCandidate
	for _, part := range strings.Split(value, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		scheme := strings.ToLower(strings.TrimSpace(kv[0]))
		if scheme != "http" && scheme != "https" {
			continue
		}
		out = append(out, proxyCandidate{scheme, strings.TrimSpace(kv[1]), ProxySourceOS, detail})
	}
	return out
}

var (
	scutilHTTPEnableRE  = regexp.MustCompile(`HTTPEnable\s*:\s*1`)
	scutilHTTPProxyRE   = regexp.MustCompile(`HTTPProxy\s*:\s*(\S+)`)
	scutilHTTPPortRE    = regexp.MustCompile(`HTTPPort\s*:\s*(\d+)`)
	scutilHTTPSEnableRE = regexp.MustCompile(`HTTPSEnable\s*:\s*1`)
	scutilHTTPSProxyRE  = regexp.MustCompile(`HTTPSProxy\s*:\s*(\S+)`)
	scutilHTTPSPortRE   = regexp.MustCompile(`HTTPSPort\s*:\s*(\d+)`)
	scutilPACEnableRE   = regexp.MustCompile(`ProxyAutoConfigEnable\s*:\s*1`)
	scutilPACURLRE      = regexp.MustCompile(`ProxyAutoConfigURLString\s*:\s*(\S+)`)
)

// detectMacOSProxy runs `scutil --proxy` and parses its key:value dump.
func detectMacOSProxy() ([]proxyCandidate, *HostProxyPAC) {
	out, err := execCommandOutput("scutil", "--proxy")
	if err != nil {
		return nil, nil
	}
	return parseScutilProxyOutput(string(out))
}

// parseScutilProxyOutput is the pure parse of `scutil --proxy` text output,
// separated from the exec call so it is testable with a canned sample.
func parseScutilProxyOutput(text string) ([]proxyCandidate, *HostProxyPAC) {
	var candidates []proxyCandidate
	if scutilHTTPEnableRE.MatchString(text) {
		if host := scutilHTTPProxyRE.FindStringSubmatch(text); host != nil {
			val := host[1]
			if port := scutilHTTPPortRE.FindStringSubmatch(text); port != nil {
				val += ":" + port[1]
			}
			candidates = append(candidates, proxyCandidate{"http", val, ProxySourceOS, "macOS scutil --proxy"})
		}
	}
	if scutilHTTPSEnableRE.MatchString(text) {
		if host := scutilHTTPSProxyRE.FindStringSubmatch(text); host != nil {
			val := host[1]
			if port := scutilHTTPSPortRE.FindStringSubmatch(text); port != nil {
				val += ":" + port[1]
			}
			candidates = append(candidates, proxyCandidate{"https", val, ProxySourceOS, "macOS scutil --proxy"})
		}
	}
	var pac *HostProxyPAC
	if scutilPACEnableRE.MatchString(text) {
		if u := scutilPACURLRE.FindStringSubmatch(text); u != nil {
			pac = &HostProxyPAC{URL: u[1], Source: ProxySourceOS, Detail: "macOS scutil --proxy ProxyAutoConfigURLString"}
		}
	}
	return candidates, pac
}
