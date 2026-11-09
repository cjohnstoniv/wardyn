// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// The strings that mark a file in the agent's home as Wardyn's placeholder, never a person's own
// credential: each is an inert value the proxy replaces. The control plane's constants of the same
// value are the source; an api test holds the two lists equal.
const (
	// ScrubSentinelClaude is the access token in a managed Claude credentials file.
	ScrubSentinelClaude = "sk-ant-oat01-wardyn-inert-sentinel-proxy-injects-the-live-token"
	// ScrubSentinelProxyInjected is Codex's API key and the AWS SSO placeholder token.
	ScrubSentinelProxyInjected = "wardyn-proxy-injected"
	// ScrubSentinelAzure is the Azure CLI token cache's access token.
	ScrubSentinelAzure = "wardyn-proxy-injects-azure-token"
	// ScrubSentinelKubectl is the kubeconfig's bearer token.
	ScrubSentinelKubectl = "wardyn-proxy-injects-kubectl-token"
	// ScrubSentinelAWSAccessKeyID and ScrubSentinelAWSSecretAccessKey are the re-signed AWS lane's
	// placeholder key pair.
	ScrubSentinelAWSAccessKeyID     = "WARDYNRESIGNSATPROXY"
	ScrubSentinelAWSSecretAccessKey = "wardyn-proxy-re-signs-every-request-0000"
	// ScrubAWSProfile names the [profile] and [sso-session] blocks Wardyn writes into ~/.aws/config.
	ScrubAWSProfile = "wardyn"
)

// ScrubSentinels is every placeholder string the content rule looks for.
var ScrubSentinels = []string{ScrubSentinelClaude, ScrubSentinelProxyInjected, ScrubSentinelAzure,
	ScrubSentinelKubectl, ScrubSentinelAWSAccessKeyID, ScrubSentinelAWSSecretAccessKey}

// MacScrubHomeFiles are the files under the agent's home a run's prep can write a credential
// placeholder into. One is removed only when it holds a ScrubSentinels string; without one it is the
// person's own and stays. .aws/config is the exception: only Wardyn's ScrubAWSProfile sections leave
// it (scrubAWSConfig). Artifact mirror records are URL-only and not listed: they stay.
var MacScrubHomeFiles = []string{
	".claude/.credentials.json",
	".claude-run/.credentials.json",
	".codex/auth.json",
	".azure/azureProfile.json",
	".azure/msal_token_cache.json",
	".kube/config",
	".aws/config",
	".aws/wardyn-placeholder-credentials.json",
}

// MacScrubHomeDirs are removed whole: the AWS CLI's sign-in caches, whose files are opaque, so a
// person's sign-in made in a run cannot be told from Wardyn's placeholder.
var MacScrubHomeDirs = []string{".aws/sso/cache", ".aws/cli/cache"}

// MacMCPTableRecord, root's alone, names the Codex [mcp_servers.<name>] tables Init's run carries,
// so a scrub after a hard stop still knows the ones that hold no broker URL.
const MacMCPTableRecord = "/var/db/wardyn/mcp-tables"

// The marker directory's files: the marker of this boot's Init, and the order of its pauses.
const (
	initMarkerFile = "wardyn-guest-init"
	pauseFenceFile = "wardyn-pause-fence"
)

// The ScrubReport.Removed keys.
const (
	ScrubManagedFiles     = "managed_files"
	ScrubCAFiles          = "ca_files"
	ScrubCAUntrusted      = "ca_untrusted"
	ScrubCAUntrustRefused = "ca_untrust_refused"
	ScrubCAUntrustFailed  = "ca_untrust_failed"
	ScrubMCPTables        = "mcp_tables"
	ScrubPlaceholderFiles = "placeholder_files"
	ScrubAWSCaches        = "aws_caches"
	ScrubGitRoutes        = "git_routes"
	ScrubRunDirs          = "run_dirs"
	ScrubDesktopPassword  = "desktop_password"
	ScrubAWSProfiles      = "aws_profiles"
	// ScrubAWSConfigMalformed counts a ~/.aws/config left whole, unread as sections, that names
	// Wardyn's profile: the scrub fails, so the record shows it.
	ScrubAWSConfigMalformed = "aws_config_malformed"
)

// scrubReadMax bounds a file the scrub reads: every file Wardyn writes is far smaller, and a larger
// one is the person's.
const scrubReadMax = 4 << 20

// MacScrub removes from a macOS guest what a run wrote that is not the person's: managed settings,
// the run CA's files and keychain trust, MCP broker routes, credential placeholders, the AWS CLI
// caches, git's broker routes, the desktop password and the run directories. The same rules run at
// a graceful stop and at boot, before the agent serves; the person's own files and sign-ins stay.
// Everything under the agent's home is reached through an os.Root, so a symlink the agent made
// cannot point a removal or a rewrite outside it, and a rewrite replaces the file rather than writing
// through a hard link.
type MacScrub struct {
	// Root prefixes every guest path; "" is "/" (tests use a temporary folder).
	Root string
	// OutDir and MarkerDir are the Server's, as guest paths.
	OutDir, MarkerDir string
	// AgentUID and AgentGID own a file the scrub rewrites in the agent's home; -1 keeps the scrub's.
	AgentUID, AgentGID int
	// Keychain removes the recorded run CAs.
	Keychain MacKeychain
	// SetDesktopPassword sets Screen Sharing's password (Desktop.SetPassword); nil sets none.
	SetDesktopPassword func(pw string) error
	// MCPRecord is MacMCPTableRecord; "" is that path under Root.
	MCPRecord string
}

func (m MacScrub) path(p string) string { return filepath.Join(cmp.Or(m.Root, "/"), p) }

func (m MacScrub) mcpRecord() string { return cmp.Or(m.MCPRecord, m.path(MacMCPTableRecord)) }

// BootScrubDue says whether this boot's Init has not happened yet. The agent's service can restart
// within a boot; a scrub then would remove the running run's settings and CA.
func BootScrubDue(markerDir string) bool {
	_, err := os.Lstat(filepath.Join(markerDir, initMarkerFile))
	return errors.Is(err, fs.ErrNotExist)
}

type scrubbing struct {
	removed map[string]int
	cas     []string
	failed  bool
}

func (s *scrubbing) add(key string, n int) {
	if n > 0 {
		s.removed[key] += n
	}
}

// fail records a step's error; an absent path is not one.
func (s *scrubbing) fail(what string, err error) {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	s.failed = true
	slog.Warn("guest agent: scrub", "step", what, "err", err)
}

func (s *scrubbing) remove(key, path string) {
	err := os.Remove(path)
	if err == nil {
		s.add(key, 1)
	}
	s.fail(path, err)
}

// Scrub runs every rule and reports. when is ScrubAtStop or ScrubAtBoot; only the boot scrub removes
// the pause fence, which the stopping agent still keeps. Neither removes the Init marker: at boot
// it is absent (BootScrubDue), and at a stop it keeps a second Init out.
func (m MacScrub) Scrub(ctx context.Context, when string) ScrubReport {
	s := &scrubbing{removed: map[string]int{}}
	for _, p := range []string{MacClaudeCodeManagedSettingsPath, MacCodexRequirementsPath} {
		s.remove(ScrubManagedFiles, m.path(p))
	}
	for _, p := range []string{MacClaudeCodeManagedMCPPath, MacClaudeMCPConfigPath} {
		s.remove(ScrubMCPTables, m.path(p))
	}
	m.scrubRunDirs(s, when)
	m.scrubHome(s)
	u, err := m.Keychain.UntrustRecorded(ctx)
	s.fail("run CA records", err)
	s.add(ScrubCAUntrusted, len(u.Removed))
	s.add(ScrubCAUntrustRefused, len(u.Refused))
	s.add(ScrubCAUntrustFailed, len(u.Failed))
	s.cas = append(append(append(s.cas, u.Removed...), u.Refused...), u.Failed...)
	if len(u.Refused)+len(u.Failed) > 0 {
		s.failed = true
	}
	if m.SetDesktopPassword != nil {
		err := m.SetDesktopPassword(rand.Text()[:vncPasswordLen])
		if err == nil {
			s.add(ScrubDesktopPassword, 1)
		}
		s.fail("desktop password", err)
	}
	r := ScrubReport{When: when, Outcome: ScrubDone, CAs: s.cas}
	if len(s.removed) > 0 {
		r.Removed = s.removed
	}
	if s.failed {
		r.Outcome = ScrubFailed
	}
	return r
}

// scrubRunDirs removes the delivered-secret directory, the run's settings directory and every detached
// exec's output; the output directory itself stays, since the agent writes there.
func (m MacScrub) scrubRunDirs(s *scrubbing, when string) {
	for _, dir := range []string{MacRunDir, MacSessionEnvDir} {
		if _, err := os.Lstat(m.path(dir)); err == nil {
			err = os.RemoveAll(m.path(dir))
			if err == nil {
				s.add(ScrubRunDirs, 1)
			}
			s.fail(dir, err)
		}
	}
	if m.OutDir != "" {
		entries, err := os.ReadDir(m.path(m.OutDir))
		s.fail(m.OutDir, err)
		for _, e := range entries {
			err := os.RemoveAll(filepath.Join(m.path(m.OutDir), e.Name()))
			if err == nil {
				s.add(ScrubRunDirs, 1)
			}
			s.fail(m.OutDir, err)
		}
	}
	if when == ScrubAtBoot && m.MarkerDir != "" {
		s.remove(ScrubRunDirs, filepath.Join(m.path(m.MarkerDir), pauseFenceFile))
	}
}

func (m MacScrub) scrubHome(s *scrubbing) {
	home, err := os.OpenRoot(m.path(MacAgentHome))
	if err != nil {
		s.fail(MacAgentHome, err)
		return
	}
	defer home.Close()
	for _, p := range []string{MacCABundlePath, MacCAFullPath} {
		rel := strings.TrimPrefix(p, MacAgentHome+"/")
		err := home.Remove(rel)
		if err == nil {
			s.add(ScrubCAFiles, 1)
		}
		s.fail(p, err)
	}
	m.scrubAWSConfig(home, s)
	for _, rel := range MacScrubHomeFiles {
		if rel == awsConfig {
			continue
		}
		b, err := readHomeFile(home, rel)
		s.fail(rel, err)
		if err != nil || !wardynPlaceholder(b) {
			continue
		}
		err = home.Remove(rel)
		if err == nil {
			s.add(ScrubPlaceholderFiles, 1)
		}
		s.fail(rel, err)
	}
	for _, rel := range MacScrubHomeDirs {
		if _, err := home.Lstat(rel); err != nil {
			s.fail(rel, err)
			continue
		}
		err := home.RemoveAll(rel)
		if err == nil {
			s.add(ScrubAWSCaches, 1)
		}
		s.fail(rel, err)
	}
	n, err := m.rewrite(home, ".gitconfig", func(sec []string) bool { return brokerRoute(sec[0]) })
	s.add(ScrubGitRoutes, n)
	s.fail(".gitconfig", err)
	recorded := m.recordedMCPTables(s)
	n, err = m.rewrite(home, ".codex/config.toml", func(sec []string) bool { return wardynMCPTable(sec, recorded) })
	s.add(ScrubMCPTables, n)
	s.fail(".codex/config.toml", err)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		s.fail(MacMCPTableRecord, os.Remove(m.mcpRecord()))
	}
}

// readHomeFile is a regular file's content, never a symlink's target; a larger file than any Wardyn
// writes reads as empty.
func readHomeFile(home *os.Root, rel string) ([]byte, error) {
	fi, err := home.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > scrubReadMax {
		return nil, nil
	}
	return home.ReadFile(rel)
}

// wardynPlaceholder is the content rule: b holds a placeholder string.
func wardynPlaceholder(b []byte) bool {
	for _, s := range ScrubSentinels {
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	return false
}

const awsConfig = ".aws/config"

// scrubAWSConfig removes Wardyn's [profile wardyn] and [sso-session wardyn] sections from
// ~/.aws/config and keeps every other section byte for byte; the file goes only when nothing but
// blank lines and comments is left. A file it cannot read as sections is left whole; when it names
// Wardyn's profile the scrub fails.
func (m MacScrub) scrubAWSConfig(home *os.Root, s *scrubbing) {
	b, err := readHomeFile(home, awsConfig)
	s.fail(awsConfig, err)
	if err != nil || b == nil {
		return
	}
	if !wellFormedINI(string(b)) {
		if namesWardynAWS(string(b)) {
			s.add(ScrubAWSConfigMalformed, 1)
			s.failed = true
		}
		return
	}
	drop := func(sec []string) bool { return wardynAWSSection(sec[0]) }
	conf, n := dropSections(string(b), drop)
	switch {
	case n == 0:
		return
	case onlyComments(conf):
		err = home.Remove(awsConfig)
		if err == nil {
			s.add(ScrubAWSProfiles, n)
			s.add(ScrubPlaceholderFiles, 1)
		}
	default:
		n, err = m.rewrite(home, awsConfig, drop)
		s.add(ScrubAWSProfiles, n)
	}
	s.fail(awsConfig, err)
}

// wardynAWSSection is a [profile wardyn] or [sso-session wardyn] header: names Wardyn reserves.
func wardynAWSSection(header string) bool {
	h := strings.TrimSpace(header)
	if !strings.HasPrefix(h, "[") || !strings.HasSuffix(h, "]") {
		return false
	}
	switch strings.Join(strings.Fields(h[1:len(h)-1]), " ") {
	case "profile " + ScrubAWSProfile, "sso-session " + ScrubAWSProfile:
		return true
	}
	return false
}

// namesWardynAWS is text with a Wardyn profile or sso-session header, closed or not.
func namesWardynAWS(conf string) bool {
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(l), "["), "]")
		switch strings.Join(strings.Fields(t), " ") {
		case "profile " + ScrubAWSProfile, "sso-session " + ScrubAWSProfile:
			return true
		}
	}
	return false
}

func iniComment(l string) bool {
	t := strings.TrimSpace(l)
	return t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";")
}

// wellFormedINI is text whose lines before the first section are blank or comments, and whose
// every section header is closed.
func wellFormedINI(conf string) bool {
	if !utf8.ValidString(conf) {
		return false
	}
	inSection := false
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "["):
			if !strings.HasSuffix(t, "]") {
				return false
			}
			inSection = true
		case !inSection && !iniComment(l):
			return false
		}
	}
	return true
}

func onlyComments(conf string) bool {
	for _, l := range strings.Split(conf, "\n") {
		if !iniComment(l) {
			return false
		}
	}
	return true
}

// rewrite drops from an INI- or TOML-shaped file in the agent's home every section drop names, and
// answers how many it dropped. The new content replaces the file through a rename, the agent's and at
// the old mode, so a hard link to the old file is never written through.
func (m MacScrub) rewrite(home *os.Root, rel string, drop func(section []string) bool) (int, error) {
	b, err := readHomeFile(home, rel)
	if err != nil || b == nil {
		return 0, err
	}
	conf, n := dropSections(string(b), drop)
	if n == 0 {
		return 0, nil
	}
	fi, err := home.Lstat(rel)
	if err != nil {
		return 0, err
	}
	tmp := rel + ".wardyn-scrub"
	_ = home.Remove(tmp)
	f, err := home.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	_, err = f.WriteString(conf)
	if err == nil && m.AgentUID >= 0 {
		err = f.Chown(m.AgentUID, m.AgentGID)
	}
	if err == nil {
		err = f.Chmod(fi.Mode().Perm())
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = home.Rename(tmp, rel)
	}
	if err != nil {
		_ = home.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// dropSections splits conf at its section headers (a line that starts with "[") and leaves out each
// section drop names; the lines before the first header always stay, and every kept line keeps its
// bytes, its line ending included.
func dropSections(conf string, drop func(section []string) bool) (string, int) {
	secs := [][]string{nil}
	for _, l := range strings.SplitAfter(conf, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			secs = append(secs, nil)
		}
		secs[len(secs)-1] = append(secs[len(secs)-1], l)
	}
	var out strings.Builder
	n := 0
	for i, sec := range secs {
		if i > 0 && drop(sec) {
			n++
			continue
		}
		for _, l := range sec {
			out.WriteString(l)
		}
	}
	return out.String(), n
}

// brokerRoute is a git [url "<base>/wardyn/gh/..."] or [url "<base>/wardyn/git/..."] header, the
// routes prep points at the run proxy's brokers.
func brokerRoute(header string) bool {
	h := strings.TrimSpace(header)
	if !strings.HasPrefix(h, "[url") {
		return false
	}
	_, url, ok := strings.Cut(h, `"`)
	url, _, ok2 := strings.Cut(url, `"`)
	return ok && ok2 && (strings.Contains(url, "/wardyn/gh/") || strings.Contains(url, "/wardyn/git/"))
}

// wardynMCPTable is a Codex [mcp_servers.<name>] table prep wrote: one whose url is a run proxy's MCP
// broker, or one a run's Init recorded.
func wardynMCPTable(sec []string, recorded map[string]bool) bool {
	m := mcpTable.FindStringSubmatch(strings.Join(strings.Fields(sec[0]), ""))
	if m == nil {
		return false
	}
	if recorded[m[1]] {
		return true
	}
	for _, l := range sec[1:] {
		if k, v, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == "url" && strings.Contains(v, "/wardyn/mcp/") {
			return true
		}
	}
	return false
}

// RecordMCPTables adds to MacMCPTableRecord the Codex MCP table names a run's env carries: its
// broker servers (WARDYN_MCP_SERVERS_JSON) and its local ones (WARDYN_MCP_STDIO_TOML). Only names,
// never a URL: a URL holds the run's MCP token.
func (m MacScrub) RecordMCPTables(envs ...map[string]string) error {
	var names []string
	for _, env := range envs {
		for _, e := range mcpURLEntry.FindAllStringSubmatch(env["WARDYN_MCP_SERVERS_JSON"], -1) {
			names = append(names, e[1])
		}
		for _, l := range strings.Split(env["WARDYN_MCP_STDIO_TOML"], "\n") {
			if t := mcpTable.FindStringSubmatch(strings.TrimSpace(l)); t != nil {
				names = append(names, t[1])
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.mcpRecord()), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(m.mcpRecord(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strings.Join(names, "\n") + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (m MacScrub) recordedMCPTables(s *scrubbing) map[string]bool {
	b, err := os.ReadFile(m.mcpRecord())
	s.fail(MacMCPTableRecord, err)
	out := map[string]bool{}
	for _, n := range strings.Fields(string(b)) {
		out[n] = true
	}
	return out
}
