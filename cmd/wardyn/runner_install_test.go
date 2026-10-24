// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/releasesig"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

// installRig is setupRig with a signed release: the fake organisation serves <base>/v<version>/ with
// SHA256SUMS, its bundle and the runner, signed by a test Fulcio and a virtual Rekor, never the real ones.
type installRig struct {
	*setupRig
	signer *releasesigtest.Signer
}

func newInstallRig(t *testing.T) *installRig {
	r := &installRig{setupRig: newSetupRig(t), signer: releasesigtest.New(t)}
	oldTrust, oldArch := releaseTrust, setupGOARCH
	t.Cleanup(func() { releaseTrust, setupGOARCH = oldTrust, oldArch })
	releaseTrust = func() (root.TrustedMaterial, error) { return r.signer.TrustedMaterial(), nil }
	setupGOARCH = "amd64"
	return r
}

// publish serves version's release with runner as its linux/amd64 runner; edit changes what is served
// after signing, signedAs how SHA256SUMS is signed.
func (r *installRig) publish(version string, runner []byte, signedAs releasesigtest.Options, edit func(files map[string][]byte)) {
	sum := sha256.Sum256(runner)
	sums := []byte(hex.EncodeToString(sum[:]) + "  ./wardyn-runnerd-linux-amd64\n" + strings.Repeat("0", 64) + "  ./wardyn-runnerd-darwin-arm64\n")
	files := map[string][]byte{"SHA256SUMS": sums, "SHA256SUMS.sigstore.json": r.signer.Sign(sums, signedAs), "wardyn-runnerd-linux-amd64": runner}
	if edit != nil {
		edit(files)
	}
	for name, b := range files {
		r.assets["/release/v"+version+"/"+name] = b
	}
}

func releaseSigner(version string) releasesigtest.Options {
	return releasesigtest.Options{Identity: releasesig.Identity(version), Issuer: releasesig.Issuer, Repository: releasesig.Repository}
}

// fakeRunner is a wardyn-runnerd of version: --version says it (fails for ""), and setup does what wardyn-runnerd setup
// does to the binary (read itself first, as it may be .previous, keep the old one as .previous and rename
// itself into place), records the call and
// writes the status a started service would, connected unless starts is false.
func (r *installRig) fakeRunner(version string, starts bool) []byte {
	state := `{"state":"connected","pid":1,"since":"%s"}`
	if !starts {
		state = `{"state":"offline","pid":1,"since":"%s","last_error":"` + version + ` refused to start"}`
	}
	dst := installedRunnerd()
	return []byte(fmt.Sprintf(`#!/bin/sh
[ "$1" = --version ] && { [ -n "%[1]s" ] || exit 1; echo "wardyn-runnerd version %[1]s"; exit 0; }
case "%[1]s" in 0.9.*) case "$*" in *--org-ca-file*) echo "unknown flag: --org-ca-file" >&2; exit 1;; esac;; esac
echo "%[1]s $*" >> %[2]s/calls
if ! /usr/bin/cmp -s "$0" %[3]s; then
  /bin/cp "$0" %[3]s.tmp
  [ -f %[3]s ] && /bin/cp %[3]s %[3]s.p && /bin/mv %[3]s.p %[3]s.previous
  /bin/mv %[3]s.tmp %[3]s
fi
/bin/mkdir -p %[4]s
printf '%[5]s' "$(/bin/date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" > %[4]s/status.json
`, version, r.rec, dst, r.stateDir(), state))
}

// installed puts runner in place as an earlier setup would have: the binary, the identity and the service's
// organisation.
func (r *installRig) installed(runner []byte) {
	r.t.Helper()
	id := fmt.Sprintf(`{"runner_id":%q,"key_fingerprint":"fp-1","org_url_sha256":%q}`, r.runnerID, federation.OrgURLSHA256(r.srv.URL))
	for path, body := range map[string]string{
		installedRunnerd():                                        string(runner),
		filepath.Join(r.stateDir(), "runner.json"):                id,
		filepath.Join(r.home, ".config", "wardyn", "runnerd.env"): "WARDYN_RUNNER_ORG_URL=" + r.srv.URL + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *installRig) cli(args ...string) (string, error) {
	r.t.Helper()
	root := rootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(append([]string{"runner"}, args...))
	err := root.Execute()
	return out.String(), err
}

func (r *installRig) installedIs(want []byte) bool {
	got, err := os.ReadFile(installedRunnerd())
	return err == nil && string(got) == string(want)
}

// A rerun of install on a computer with a runner upgrades it: verified, swapped, started.
func TestRunnerInstallRerunUpgrades(t *testing.T) {
	r := newInstallRig(t)
	old, next := r.fakeRunner("0.10.0", true), r.fakeRunner("0.10.1", true)
	r.installed(old)
	r.publish("0.10.1", next, releaseSigner("0.10.1"), nil)
	out, err := r.cli("install", "--version", "0.10.1")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	prev, _ := os.ReadFile(installedRunnerd() + ".previous")
	if !r.installedIs(next) || string(prev) != string(old) || !strings.Contains(out, fmt.Sprintf(copyUpgradeDone, "0.10.1")) {
		t.Fatalf("after the upgrade the runner is not v0.10.1 with v0.10.0 kept:\n%s", out)
	}
	if !strings.Contains(out, releasesig.Identity("0.10.1")) || r.record("calls") != "0.10.1 setup --org "+r.srv.URL+"\n" {
		t.Fatalf("output %q, calls %q", out, r.record("calls"))
	}
}

func TestRunnerUpgradeRollsBackAFailedStart(t *testing.T) {
	r := newInstallRig(t)
	old, broken := r.fakeRunner("0.10.0", true), r.fakeRunner("0.10.1", false)
	r.installed(old)
	r.publish("0.10.1", broken, releaseSigner("0.10.1"), nil)
	out, err := r.cli("upgrade", "--version", "0.10.1")
	if err == nil || !strings.Contains(err.Error(), "so the runner is back on 0.10.0") || !strings.Contains(err.Error(), "0.10.1 refused to start") {
		t.Fatalf("a runner that did not start: err = %v\n%s", err, out)
	}
	if !r.installedIs(old) {
		t.Fatalf("the previous runner was not put back:\n%s", out)
	}
	if calls := r.record("calls"); calls != "0.10.1 setup --org "+r.srv.URL+"\n0.10.0 setup --org "+r.srv.URL+"\n" {
		t.Fatalf("calls = %q, want the new setup then the previous one's", calls)
	}
}

// The rollback runs the previous binary, which may predate --org-ca-file (a 0.9 runner upgraded to 0.10 on
// an install with a stored organisation CA): it must get the flags that version has.
func TestRunnerUpgradeRollsBackAnOlderRunnerWithoutOrgCAFile(t *testing.T) {
	r := newInstallRig(t)
	old, broken := r.fakeRunner("0.9.8", true), r.fakeRunner("0.10.0", false)
	r.installed(old)
	r.publish("0.10.0", broken, releaseSigner("0.10.0"), nil)
	anchor, _, err := anchorFile(r.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte("pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := r.cli("upgrade", "--version", "0.10.0")
	if err == nil || !strings.Contains(err.Error(), "so the runner is back on 0.9.8") {
		t.Fatalf("a runner that did not start: err = %v\n%s", err, out)
	}
	if !r.installedIs(old) {
		t.Fatalf("the previous runner was not put back:\n%s", out)
	}
	if calls := r.record("calls"); calls != "0.10.0 setup --org "+r.srv.URL+" --org-ca-file "+anchor+"\n0.9.8 setup --org "+r.srv.URL+"\n" {
		t.Fatalf("calls = %q, want the new setup with the anchor then the older one's without", calls)
	}
}

func TestRunnerUpgradeAtTheSameVersionIsANoOp(t *testing.T) {
	r := newInstallRig(t)
	current := r.fakeRunner("0.10.1", true)
	r.installed(current)
	r.publish("0.10.1", current, releaseSigner("0.10.1"), nil)
	for _, verb := range []string{"upgrade", "install"} {
		out, err := r.cli(verb, "--version", "0.10.1")
		if err != nil || !strings.Contains(out, fmt.Sprintf(copyUpgradeCurrent, "0.10.1")) {
			t.Fatalf("%s at the installed version: %v\n%s", verb, err, out)
		}
	}
	if got := r.called("GET /release"); len(got) != 0 || r.record("calls") != "" {
		t.Fatalf("a no-op downloaded %v or ran setup %q", got, r.record("calls"))
	}
}

// A runner whose version cannot be read and whose bytes are already the release's keeps no new .previous,
// so a failed start must not put back an older .previous left by an earlier upgrade.
func TestRunnerUpgradeNeverRestoresAStalePrevious(t *testing.T) {
	r := newInstallRig(t)
	broken, stale := r.fakeRunner("", false), r.fakeRunner("0.9.0", true)
	r.installed(broken)
	if err := os.WriteFile(installedRunnerd()+".previous", stale, 0o700); err != nil {
		t.Fatal(err)
	}
	r.publish("0.10.1", broken, releaseSigner("0.10.1"), nil)
	out, err := r.cli("upgrade", "--version", "0.10.1")
	if err == nil || !strings.Contains(err.Error(), "kept no previous runner to put back") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !r.installedIs(broken) || r.record("calls") != " setup --org "+r.srv.URL+"\n" {
		t.Fatalf("the stale v0.9.0 .previous was put back: calls %q\n%s", r.record("calls"), out)
	}
}

// The organisation's advertised mirror is held to https, as the flag and the managed preference are.
func TestRunnerUpgradeRefusesAPlainHTTPAdvertisedMirror(t *testing.T) {
	r := newInstallRig(t)
	old := r.fakeRunner("0.10.0", true)
	r.installed(old)
	r.advertised = "http://mirror.example"
	out, err := r.cli("upgrade", "--version", "0.10.1")
	if err == nil || !strings.Contains(err.Error(), "the organisation's advertised mirror (/healthz.cli_artifact_base)") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got := r.called("GET /release"); len(got) != 0 || !r.installedIs(old) {
		t.Fatalf("downloaded %v from the organisation's own origin instead", got)
	}
}

// Every refusal leaves the installed runner alone and runs nothing.
func TestRunnerUpgradeRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		signedAs releasesigtest.Options
		edit     func(map[string][]byte)
		want     string
	}{
		"tampered SHA256SUMS": {releaseSigner("0.10.1"), func(f map[string][]byte) {
			f["SHA256SUMS"] = append([]byte(nil), f["SHA256SUMS"]...)
			f["SHA256SUMS"][0] ^= 1
		},
			releasesig.ErrSignature.Error()},
		"tampered asset":   {releaseSigner("0.10.1"), func(f map[string][]byte) { f["wardyn-runnerd-linux-amd64"] = []byte("#!/bin/sh\n") }, "but the signed SHA256SUMS lists"},
		"another version":  {releaseSigner("0.10.0"), nil, releasesig.ErrIdentity.Error()},
		"another workflow": {releasesigtest.Options{Identity: releasesig.Repository + "/.github/workflows/ci.yml@refs/tags/v0.10.1", Issuer: releasesig.Issuer, Repository: releasesig.Repository}, nil, releasesig.ErrIdentity.Error()},
		"another issuer":   {releasesigtest.Options{Identity: releasesig.Identity("0.10.1"), Issuer: "https://accounts.google.com", Repository: releasesig.Repository}, nil, releasesig.ErrIdentity.Error()},
		"missing bundle":   {releaseSigner("0.10.1"), func(f map[string][]byte) { delete(f, "SHA256SUMS.sigstore.json") }, "signature bundle missing: "},
		"asset missing on mirror": {releaseSigner("0.10.1"), func(f map[string][]byte) { delete(f, "wardyn-runnerd-linux-amd64") },
			"asset missing on mirror: "},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInstallRig(t)
			old := r.fakeRunner("0.10.0", true)
			r.installed(old)
			r.publish("0.10.1", r.fakeRunner("0.10.1", true), tc.signedAs, tc.edit)
			out, err := r.cli("upgrade", "--version", "0.10.1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q\n%s", err, tc.want, out)
			}
			if !r.installedIs(old) || r.record("calls") != "" {
				t.Fatalf("a refused release changed the runner or ran setup %q", r.record("calls"))
			}
		})
	}
}

// On a computer with no runner, install is setup with the signed release's runner.
func TestRunnerInstallSetsUpWithTheSignedRunner(t *testing.T) {
	r := newInstallRig(t)
	bin, _ := r.fakeRunnerd(r.binDir, 0)
	script, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	r.publish("0.10.1", script, releaseSigner("0.10.1"), nil)
	out, err := r.cli("install", "--url", r.srv.URL, "--token", "wdn_personal", "--version", "0.10.1")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if r.mints != 1 || !r.claimed || !strings.Contains(r.record("argv"), "setup\n--org\n") || !strings.Contains(out, releasesig.Identity("0.10.1")) {
		t.Fatalf("mints=%d claimed=%v argv=%q\n%s", r.mints, r.claimed, r.record("argv"), out)
	}
}

func TestRunnerOfferLine(t *testing.T) {
	r := newInstallRig(t)
	if got := runnerOfferLine(context.Background(), "0.10.1"); got != "" {
		t.Fatalf("no runner installed, yet %q", got)
	}
	r.installed(r.fakeRunner("0.10.0", true))
	if got, want := runnerOfferLine(context.Background(), "0.10.1+abc1234"), fmt.Sprintf(copyNoticeRunnerOlder, "0.10.0", "0.10.1", "0.10.1"); got != want {
		t.Fatalf("offer = %q, want %q", got, want)
	}
	if got := runnerOfferLine(context.Background(), "0.10.0"); got != "" {
		t.Fatalf("a current runner was offered an upgrade: %q", got)
	}
}
