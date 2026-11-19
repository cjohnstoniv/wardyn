// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every value below is fake: the scrub's tests never hold a real credential.

const scrubOutDir = "/var/run/wardyn-guest"

// scrubOn is a MacScrub on a temporary root whose security is r.
func scrubOn(t *testing.T, r CmdRunner) (MacScrub, string) {
	t.Helper()
	root := t.TempDir()
	return MacScrub{Root: root, OutDir: scrubOutDir, MarkerDir: "/var/run", AgentUID: -1, AgentGID: -1,
		Keychain: MacKeychain{Run: r, Dir: t.TempDir(), RecordDir: filepath.Join(root, MacRunCARecordDir)}}, root
}

func put(t *testing.T, root, guestPath, content string) {
	t.Helper()
	p := filepath.Join(root, guestPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func homePath(rel string) string { return MacAgentHome + "/" + rel }

func exists(root, guestPath string) bool {
	_, err := os.Lstat(filepath.Join(root, guestPath))
	return err == nil
}

func read(t *testing.T, root, guestPath string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, guestPath))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScrubRemovesManagedFiles(t *testing.T) {
	m, root := scrubOn(t, &fakeRunner{})
	for _, p := range []string{MacClaudeCodeManagedSettingsPath, MacCodexRequirementsPath, MacClaudeCodeManagedMCPPath, MacClaudeMCPConfigPath} {
		put(t, root, p, "{}")
	}
	put(t, root, MacCodexManagedDir+"/someone-elses.toml", "x = 1")
	r := m.Scrub(context.Background(), ScrubAtStop)
	for _, p := range []string{MacClaudeCodeManagedSettingsPath, MacCodexRequirementsPath, MacClaudeCodeManagedMCPPath, MacClaudeMCPConfigPath} {
		if exists(root, p) {
			t.Errorf("%s is left", p)
		}
	}
	if !exists(root, MacCodexManagedDir+"/someone-elses.toml") {
		t.Error("a file the run did not write is removed")
	}
	if r.When != ScrubAtStop || r.Outcome != ScrubDone || r.Removed[ScrubManagedFiles] != 2 || r.Removed[ScrubMCPTables] != 2 {
		t.Errorf("report = %+v", r)
	}
}

func TestScrubStripsWardynMCPTables(t *testing.T) {
	m, root := scrubOn(t, &fakeRunner{})
	err := m.RecordMCPTables(map[string]string{"WARDYN_MCP_STDIO_TOML": "[mcp_servers.local]\ncommand = \"npx\"\nargs = []\nenv_vars = []\n"},
		map[string]string{"WARDYN_MCP_SERVERS_JSON": `{"gh":"http://wardyn-proxy:3128/wardyn/mcp/fake-token/gh"}`})
	if err != nil {
		t.Fatal(err)
	}
	if rec := read(t, root, MacMCPTableRecord); strings.Contains(rec, "fake-token") || !strings.Contains(rec, "gh") || !strings.Contains(rec, "local") {
		t.Fatalf("the record holds %q: want the names alone", rec)
	}
	conf := "model = \"m\"\n" +
		"[mcp_servers.gh]\nurl = \"http://wardyn-proxy:3128/wardyn/mcp/fake-token/gh\"\n" +
		"[mcp_servers.old]\nurl = \"http://wardyn-proxy:3128/wardyn/mcp/older-fake/old\"\n" +
		"[mcp_servers.local]\ncommand = \"npx\"\n" +
		"[mcp_servers.mine]\ncommand = \"my-server\"\n" +
		"[profiles.work]\nmodel = \"n\"\n"
	put(t, root, homePath(".codex/config.toml"), conf)
	if err := os.Chmod(filepath.Join(root, homePath(".codex/config.toml")), 0o600); err != nil {
		t.Fatal(err)
	}
	r := m.Scrub(context.Background(), ScrubAtBoot)
	want := "model = \"m\"\n[mcp_servers.mine]\ncommand = \"my-server\"\n[profiles.work]\nmodel = \"n\"\n"
	if got := read(t, root, homePath(".codex/config.toml")); got != want {
		t.Errorf("config.toml =\n%s\nwant\n%s", got, want)
	}
	if fi, _ := os.Stat(filepath.Join(root, homePath(".codex/config.toml"))); fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml mode = %v, want the old 0600", fi.Mode().Perm())
	}
	if r.Removed[ScrubMCPTables] != 3 || r.Outcome != ScrubDone {
		t.Errorf("report = %+v", r)
	}
	if exists(root, MacMCPTableRecord) {
		t.Error("the record is kept after its tables are gone")
	}
}

func TestTrustRecordsBeforeTrusting(t *testing.T) {
	ca := testCAPEM(t, "run proxy CA")
	block, _ := pem.Decode([]byte(ca))
	sum := sha256.Sum256(block.Bytes)
	dir := filepath.Join(t.TempDir(), "run-cas")
	record := filepath.Join(dir, hex.EncodeToString(sum[:])+".pem")
	var held string
	r := &fakeRunner{out: func([]string) ([]byte, error) {
		b, _ := os.ReadFile(record)
		held = string(b)
		return nil, nil
	}}
	if status, _ := (MacKeychain{Run: r, Dir: t.TempDir(), RecordDir: dir}).TrustRunCA(context.Background(), ca); status != KeychainTrusted {
		t.Fatalf("TrustRunCA = %q", status)
	}
	if held != ca {
		t.Errorf("when security ran, the record held %q, want the CA", held)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the record folder: %v %v, want root's alone", fi, err)
	}
	// A CA that cannot be recorded is not trusted: no trust the scrub does not know of.
	blocked := filepath.Join(t.TempDir(), "a-file")
	put(t, "/", blocked, "")
	r = &fakeRunner{}
	status, reason := MacKeychain{Run: r, Dir: t.TempDir(), RecordDir: blocked}.TrustRunCA(context.Background(), ca)
	if status != KeychainNotTrusted || reason != KeychainUnavailable || len(r.calls) != 0 {
		t.Errorf("unrecordable: TrustRunCA = %q %q after %d calls", status, reason, len(r.calls))
	}
}

// caDigests is a CA's SHA-256 (the record's name) and upper-case SHA-1 (security's -Z).
func caDigests(t *testing.T, ca string) (string, string) {
	t.Helper()
	block, _ := pem.Decode([]byte(ca))
	s256, s1 := sha256.Sum256(block.Bytes), sha1.Sum(block.Bytes)
	return hex.EncodeToString(s256[:]), strings.ToUpper(hex.EncodeToString(s1[:]))
}

func TestScrubUntrustsEveryRecordedRunCA(t *testing.T) {
	r := &fakeRunner{}
	m, root := scrubOn(t, r)
	one, two := testCAPEM(t, "run one"), testCAPEM(t, "run two")
	for _, ca := range []string{one, two} {
		if status, _ := m.Keychain.TrustRunCA(context.Background(), ca); status != KeychainTrusted {
			t.Fatal(status)
		}
	}
	put(t, root, MacCABundlePath, one)
	put(t, root, MacCAFullPath, one)
	sha1Gone := ""
	r.calls = nil
	r.out = func(argv []string) ([]byte, error) {
		if argv[1] == "delete-certificate" && argv[4] == sha1Gone {
			return nil, exitError(t, "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.")
		}
		return nil, nil
	}
	fp1, s1 := caDigests(t, one)
	fp2, s2 := caDigests(t, two)
	sha1Gone = s2
	rep := m.Scrub(context.Background(), ScrubAtStop)
	// Per CA, in this order: the admin trust by the recorded file, then the certificate by SHA-1.
	want := map[string][][]string{}
	for _, d := range [][2]string{{fp1, s1}, {fp2, s2}} {
		want[d[0]] = [][]string{
			{"/usr/bin/security", "remove-trusted-cert", "-d", filepath.Join(m.Keychain.RecordDir, d[0]+".pem")},
			{"/usr/bin/security", "delete-certificate", "-t", "-Z", d[1], MacSystemKeychain},
		}
	}
	if len(r.calls) != 4 {
		t.Fatalf("calls = %q, want 4", r.calls)
	}
	for i := 0; i < 4; i += 2 {
		fp := strings.TrimSuffix(filepath.Base(r.calls[i][len(r.calls[i])-1]), ".pem")
		if !slices.EqualFunc(r.calls[i:i+2], want[fp], slices.Equal) {
			t.Errorf("calls %d-%d = %q, want %q", i, i+1, r.calls[i:i+2], want[fp])
		}
	}
	if entries, _ := os.ReadDir(m.Keychain.RecordDir); len(entries) != 0 {
		t.Errorf("records left: %v", entries)
	}
	got := slices.Sorted(slices.Values(rep.CAs))
	if !slices.Equal(got, slices.Sorted(slices.Values([]string{fp1, fp2}))) {
		t.Errorf("CAs = %v, want both fingerprints", rep.CAs)
	}
	if rep.Removed[ScrubCAUntrusted] != 2 || rep.Removed[ScrubCAFiles] != 2 || rep.Outcome != ScrubDone {
		t.Errorf("report = %+v", rep)
	}
	if exists(root, MacCABundlePath) || exists(root, MacCAFullPath) {
		t.Error("the CA files are left")
	}
}

func TestScrubKeepsTheRecordWhenUntrustIsRefused(t *testing.T) {
	r := &fakeRunner{}
	m, _ := scrubOn(t, r)
	ca := testCAPEM(t, "run")
	if status, _ := m.Keychain.TrustRunCA(context.Background(), ca); status != KeychainTrusted {
		t.Fatal(status)
	}
	fp, _ := caDigests(t, ca)
	for _, refuses := range []string{"remove-trusted-cert", "delete-certificate"} {
		r.calls = nil
		r.out = func(argv []string) ([]byte, error) {
			if argv[1] == refuses {
				return nil, exitError(t, "SecTrustSettingsRemoveTrustSettings: The authorization was denied since no user interaction was possible.")
			}
			return nil, nil
		}
		rep := m.Scrub(context.Background(), ScrubAtStop)
		if rep.Outcome != ScrubFailed || rep.Removed[ScrubCAUntrustRefused] != 1 || !slices.Equal(rep.CAs, []string{fp}) {
			t.Errorf("%s refused: report = %+v", refuses, rep)
		}
		if !exists("/", filepath.Join(m.Keychain.RecordDir, fp+".pem")) {
			t.Fatalf("%s refused: the record is dropped, so no later scrub tries it again", refuses)
		}
		// A refused admin-trust removal stops there: the certificate is not deleted under its trust.
		if refuses == "remove-trusted-cert" && len(r.calls) != 1 {
			t.Errorf("after a refused remove-trusted-cert, calls = %q", r.calls)
		}
	}
	r.out = nil
	rep := m.Scrub(context.Background(), ScrubAtBoot)
	if rep.Outcome != ScrubDone || rep.Removed[ScrubCAUntrusted] != 1 || exists("/", filepath.Join(m.Keychain.RecordDir, fp+".pem")) {
		t.Errorf("retry at boot: report = %+v", rep)
	}
}

// One rule at both entries: a candidate file goes only when it holds a placeholder.
func TestScrubRemovesOnlyPlaceholderFiles(t *testing.T) {
	for _, when := range []string{ScrubAtStop, ScrubAtBoot} {
		t.Run(when, func(t *testing.T) {
			m, root := scrubOn(t, &fakeRunner{})
			gone := map[string]string{
				".claude/.credentials.json":                `{"claudeAiOauth":{"accessToken":"` + ScrubSentinelClaude + `"}}`,
				".claude-run/.credentials.json":            `{"claudeAiOauth":{"accessToken":"` + ScrubSentinelClaude + `"}}`,
				".codex/auth.json":                         `{"OPENAI_API_KEY":"` + ScrubSentinelProxyInjected + `"}`,
				".azure/msal_token_cache.json":             `{"AccessToken":{"k":{"secret":"` + ScrubSentinelAzure + `"}}}`,
				".kube/config":                             "users:\n- user:\n    token: " + ScrubSentinelKubectl + "\n",
				".aws/wardyn-placeholder-credentials.json": `{"AccessKeyId":"` + ScrubSentinelAWSAccessKeyID + `","SecretAccessKey":"` + ScrubSentinelAWSSecretAccessKey + `"}`,
				".aws/config":                              "[sso-session wardyn]\nsso_region = us-east-1\n\n[profile  wardyn ]\nsso_session = wardyn\n",
			}
			stay := map[string]string{
				".azure/azureProfile.json": `{"subscriptions":[]}`,
				"notes.txt":                "not a candidate: " + ScrubSentinelClaude,
			}
			for rel, c := range gone {
				put(t, root, homePath(rel), c)
			}
			for rel, c := range stay {
				put(t, root, homePath(rel), c)
			}
			rep := m.Scrub(context.Background(), when)
			for rel := range gone {
				if exists(root, homePath(rel)) {
					t.Errorf("%s with a placeholder is left", rel)
				}
			}
			for rel, c := range stay {
				if read(t, root, homePath(rel)) != c {
					t.Errorf("%s is changed", rel)
				}
			}
			if rep.Removed[ScrubPlaceholderFiles] != len(gone) || rep.Outcome != ScrubDone || rep.When != when {
				t.Errorf("report = %+v", rep)
			}
			all := strings.Join(slices.Collect(maps.Values(gone)), "\n")
			for _, s := range ScrubSentinels {
				if !strings.Contains(all, s) {
					t.Errorf("no file here holds %q", s)
				}
			}
			for _, rel := range MacScrubHomeFiles {
				if _, ok := gone[rel]; !ok && rel != ".azure/azureProfile.json" {
					t.Errorf("candidate %s is not exercised", rel)
				}
			}
		})
	}
}

func TestScrubKeepsAPersonsOwnClaudeAndCodexSignIn(t *testing.T) {
	m, root := scrubOn(t, &fakeRunner{})
	own := map[string]string{
		".claude/.credentials.json": `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-a-persons-own-fake"}}`,
		".codex/auth.json":          `{"tokens":{"access_token":"a-persons-own-fake"}}`,
		".aws/config":               "[profile dev]\nregion = eu-west-1\n# wardyn\n",
		".kube/config":              "users:\n- user:\n    token: a-persons-own-fake\n",
	}
	for rel, c := range own {
		put(t, root, homePath(rel), c)
	}
	rep := m.Scrub(context.Background(), ScrubAtStop)
	for rel, c := range own {
		if read(t, root, homePath(rel)) != c {
			t.Errorf("a person's own %s is changed", rel)
		}
	}
	if rep.Removed[ScrubPlaceholderFiles] != 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestScrubRemovesAWSCaches(t *testing.T) {
	m, root := scrubOn(t, &fakeRunner{})
	put(t, root, homePath(".aws/sso/cache/a.json"), `{"accessToken":"a-fake-sign-in"}`)
	put(t, root, homePath(".aws/cli/cache/b.json"), `{"Credentials":{}}`)
	put(t, root, homePath(".aws/credentials"), "[dev]\naws_access_key_id = a-persons-own-fake\n")
	rep := m.Scrub(context.Background(), ScrubAtBoot)
	if exists(root, homePath(".aws/sso/cache")) || exists(root, homePath(".aws/cli/cache")) {
		t.Error("an AWS CLI cache is left")
	}
	if !exists(root, homePath(".aws/credentials")) {
		t.Error("a file outside the caches is removed")
	}
	if rep.Removed[ScrubAWSCaches] != 2 {
		t.Errorf("report = %+v", rep)
	}
}

func TestScrubRemovesBrokerRoutesOnly(t *testing.T) {
	m, root := scrubOn(t, &fakeRunner{})
	conf := "[user]\n\tname = A Person\n" +
		"[url \"http://wardyn-proxy:3128/wardyn/gh/o/r\"]\n\tinsteadOf = https://github.com/o/r\n\tinsteadOf = git@github.com:o/r\n" +
		"[url \"http://wardyn-proxy:3128/wardyn/git/git.example/\"]\n\tinsteadOf = https://git.example/\n" +
		"[url \"https://mirror.example/\"]\n\tinsteadOf = https://github.com/\n"
	put(t, root, homePath(".gitconfig"), conf)
	// A hard link to the old file is never written through: the rewrite replaces the entry.
	outside := filepath.Join(root, "elsewhere")
	if err := os.Link(filepath.Join(root, homePath(".gitconfig")), outside); err != nil {
		t.Fatal(err)
	}
	rep := m.Scrub(context.Background(), ScrubAtStop)
	want := "[user]\n\tname = A Person\n[url \"https://mirror.example/\"]\n\tinsteadOf = https://github.com/\n"
	if got := read(t, root, homePath(".gitconfig")); got != want {
		t.Errorf(".gitconfig =\n%s\nwant\n%s", got, want)
	}
	if b, _ := os.ReadFile(outside); string(b) != conf {
		t.Error("the rewrite wrote through a hard link")
	}
	if rep.Removed[ScrubGitRoutes] != 2 || rep.Outcome != ScrubDone {
		t.Errorf("report = %+v", rep)
	}
}

func TestScrubResetsTheDesktopPassword(t *testing.T) {
	m, _ := scrubOn(t, &fakeRunner{})
	d := &Desktop{VNCSettings: filepath.Join(t.TempDir(), "vnc.txt")}
	if err := d.SetRunPassword(context.Background(), "runpw123"); err != nil {
		t.Fatal(err)
	}
	m.SetDesktopPassword = d.SetPassword
	rep := m.Scrub(context.Background(), ScrubAtStop)
	b, err := os.ReadFile(d.VNCSettings)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == string(VNCSettingsLine("runpw123")) || len(b) != len(VNCSettingsLine("x")) {
		t.Errorf("the settings file still holds the run's password, or no password: %q", b)
	}
	if rep.Removed[ScrubDesktopPassword] != 1 {
		t.Errorf("report = %+v", rep)
	}
	m.SetDesktopPassword = func(string) error { return errors.New("no settings file") }
	if rep := m.Scrub(context.Background(), ScrubAtBoot); rep.Outcome != ScrubFailed {
		t.Errorf("a password that was not reset reads %q", rep.Outcome)
	}
}

func TestScrubRemovesRunDirs(t *testing.T) {
	for _, when := range []string{ScrubAtStop, ScrubAtBoot} {
		t.Run(when, func(t *testing.T) {
			m, root := scrubOn(t, &fakeRunner{})
			put(t, root, MacRunDir+"/secrets/a-file", "a-fake-delivered-secret")
			put(t, root, MacSessionEnvPath, "export HTTPS_PROXY='http://proxy:3128'\n")
			put(t, root, scrubOutDir+"/exec-1", "output")
			put(t, root, "/var/run/"+pauseFenceFile, `{"generation":3}`)
			put(t, root, "/var/run/"+initMarkerFile, "")
			rep := m.Scrub(context.Background(), when)
			if exists(root, MacRunDir) || exists(root, MacSessionEnvDir) || exists(root, scrubOutDir+"/exec-1") {
				t.Error("a run directory's content is left")
			}
			if !exists(root, scrubOutDir) {
				t.Error("the output directory itself is removed; the agent writes there")
			}
			if exists(root, "/var/run/"+pauseFenceFile) != (when == ScrubAtStop) {
				t.Errorf("the pause fence: left = %v at %s", exists(root, "/var/run/"+pauseFenceFile), when)
			}
			if !exists(root, "/var/run/"+initMarkerFile) {
				t.Error("the Init marker is removed, so a second Init could pass")
			}
			if want := map[bool]int{true: 3, false: 4}[when == ScrubAtStop]; rep.Removed[ScrubRunDirs] != want {
				t.Errorf("report = %+v, want %d run entries", rep, want)
			}
		})
	}
}

// The boot scrub runs only before this boot's Init: a restart of the agent's service mid-run must not
// remove the running run's settings.
func TestScrubBootDueOnlyBeforeInit(t *testing.T) {
	dir := t.TempDir()
	if !BootScrubDue(dir) {
		t.Error("no marker: the boot scrub is not due")
	}
	put(t, dir, initMarkerFile, "")
	if BootScrubDue(dir) {
		t.Error("after Init: the boot scrub is due again")
	}
}

// A symlink the agent made in its home never points a removal outside it.
func TestScrubNeverLeavesTheHome(t *testing.T) {
	m, root := scrubOn(t, &fakeRunner{})
	outside := t.TempDir()
	put(t, outside, ".credentials.json", ScrubSentinelClaude)
	put(t, outside, "sso/cache/a.json", "{}")
	put(t, root, homePath("keep"), "")
	for rel, target := range map[string]string{".claude": outside, ".aws": outside, ".kube": outside} {
		if err := os.Symlink(target, filepath.Join(root, homePath(rel))); err != nil {
			t.Fatal(err)
		}
	}
	put(t, outside, "config", ScrubSentinelKubectl)
	m.Scrub(context.Background(), ScrubAtStop)
	for _, f := range []string{".credentials.json", "sso/cache/a.json", "config"} {
		if !exists(outside, f) {
			t.Errorf("%s outside the home is removed", f)
		}
	}
}

func TestScrubClientRoundTrip(t *testing.T) {
	want := ScrubReport{When: ScrubAtStop, Outcome: ScrubFailed, Removed: map[string]int{ScrubCAUntrustRefused: 1}, CAs: []string{strings.Repeat("ab", 32)}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/scrub", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(want) })
	got, err := rawGuest(t, mux).Scrub(context.Background())
	if err != nil || got.When != want.When || got.Outcome != want.Outcome || !maps.Equal(got.Removed, want.Removed) || !slices.Equal(got.CAs, want.CAs) {
		t.Fatalf("Scrub = %+v, %v", got, err)
	}
	b, _ := json.Marshal(InitResult{Scrub: &want})
	if !strings.Contains(string(b), `"scrub":{"when":"stop","outcome":"failed","removed":{"ca_untrust_refused":1},"ca_sha256":[`) {
		t.Errorf("InitResult JSON = %s", b)
	}
}

// Only Wardyn's reserved sections leave ~/.aws/config; the person's stay byte for byte.
func TestScrubAWSConfigKeepsAPersonsSections(t *testing.T) {
	wardyn := "[sso-session wardyn]\nsso_start_url = https://start.example/\nsso_region = us-east-1\n\n" +
		"[profile wardyn]\nsso_session = wardyn\noutput = json\n"
	person := "# mine\n[profile personal]\nregion = eu-west-1\nrole_arn = arn:aws:iam::000000000000:role/fake\n\n[default]\r\nregion = us-west-2"
	for _, when := range []string{ScrubAtStop, ScrubAtBoot} {
		for _, tc := range []struct {
			name, conf, want string
			gone, malformed  bool
		}{
			{"mixed", "[profile personal]\nregion = eu-west-1\n\n" + wardyn + "[default]\r\nregion = us-west-2",
				"[profile personal]\nregion = eu-west-1\n\n[default]\r\nregion = us-west-2", false, false},
			{"wardyn first", wardyn + person[len("# mine\n"):], person[len("# mine\n"):], false, false},
			{"only wardyn's", "# written by a run\n" + wardyn, "", true, false},
			{"none", person, person, false, false},
			{"malformed", "region = eu-west-1\n" + wardyn, "region = eu-west-1\n" + wardyn, false, true},
			{"unclosed header", wardyn + "[profile personal\nregion = x\n", wardyn + "[profile personal\nregion = x\n", false, true},
			{"malformed, not wardyn's", "region = eu-west-1\n[profile personal]\n", "region = eu-west-1\n[profile personal]\n", false, false},
		} {
			t.Run(when+"/"+tc.name, func(t *testing.T) {
				m, root := scrubOn(t, &fakeRunner{})
				put(t, root, homePath(awsConfig), tc.conf)
				p := filepath.Join(root, homePath(awsConfig))
				if err := os.Chmod(p, 0o600); err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-time.Hour).Truncate(time.Second)
				if err := os.Chtimes(p, old, old); err != nil {
					t.Fatal(err)
				}
				rep := m.Scrub(context.Background(), when)
				if tc.gone {
					if exists(root, homePath(awsConfig)) {
						t.Fatal("a config holding only Wardyn's sections is left")
					}
					return
				}
				if got := read(t, root, homePath(awsConfig)); got != tc.want {
					t.Fatalf("config =\n%q\nwant\n%q", got, tc.want)
				}
				fi, _ := os.Stat(p)
				if fi.Mode().Perm() != 0o600 {
					t.Errorf("mode = %v, want the old 0600", fi.Mode().Perm())
				}
				if untouched := tc.conf == tc.want; untouched != fi.ModTime().Equal(old) {
					t.Errorf("untouched = %v but mtime moved = %v", untouched, !fi.ModTime().Equal(old))
				}
				// A malformed file naming Wardyn's profile is left whole, and the record says so.
				if tc.malformed != (rep.Outcome == ScrubFailed) || tc.malformed != (rep.Removed[ScrubAWSConfigMalformed] == 1) {
					t.Errorf("report = %+v, malformed = %v", rep, tc.malformed)
				}
			})
		}
	}
}
