// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gomacho "github.com/blacktop/go-macho"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/machosign/machosigntest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// signerExec is the host's openssl, security and codesign as the signer sees them: it records each argv with
// whether its context was already done, prints list for `security list-keychains -d user` and dr after
// `codesign -d -r-`'s arrow, and fails any argv failIf picks (failIf may cancel the signing's context).
type signerExec struct {
	mu     sync.Mutex
	runs   [][]string
	done   []bool
	dr     string
	list   string
	failIf func(argv []string) bool
}

func (f *signerExec) exec(ctx context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	f.runs = append(f.runs, slices.Clone(argv))
	f.done = append(f.done, ctx.Err() != nil)
	f.mu.Unlock()
	if f.failIf != nil && f.failIf(argv) {
		return []byte("refused"), errors.New("exit status 1")
	}
	switch {
	case slices.Equal(argv, []string{"/usr/bin/security", "list-keychains", "-d", "user"}):
		return []byte(f.list), nil
	case argv[0] == "/usr/bin/codesign" && slices.Contains(argv, "-r-"):
		return []byte("Executable=" + argv[len(argv)-1] + "\ndesignated => " + f.dr + "\n"), nil
	case argv[0] == "/usr/bin/codesign" && slices.Contains(argv, "--sign"):
		p := argv[len(argv)-1]
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return nil, os.WriteFile(p, append([]byte("signed:"), b...), 0o755)
	}
	return nil, nil
}

// index is the position of the first recorded argv that starts with prefix, -1 when none does.
func (f *signerExec) index(prefix ...string) int {
	return slices.IndexFunc(f.runs, func(a []string) bool { return len(a) >= len(prefix) && slices.Equal(a[:len(prefix)], prefix) })
}

// signFixture makes this Mac's identity in a temp directory and an agent build at a temp path: a Mach-O the
// Wardyn signer takes when macho is true, else bytes only codesign (here faked) takes.
func signFixture(t *testing.T, macho bool) (dir, bin string, req string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "agent-signing")
	cert, err := macvm.AgentSigningIdentity(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte("new agent")
	if macho {
		b = machosigntest.MachO(2, 0x40)
	}
	bin = filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, bin, macvm.AgentRequirement(cert)
}

const (
	loginKC  = "/Users/me/Library/Keychains/login.keychain-db"
	systemKC = "/Library/Keychains/System.keychain"
)

// The codesign fallback names the designated requirement itself, as one argv element with exactly one "=":
// "-r=designated => " + AgentRequirement. Without it codesign derives `certificate leaf`, which the read-back
// refuses. For the call it puts the signing keychain first on the login's search list, after reading the list,
// and restores the list it read afterwards, before the keychain is deleted: when codesign fails too, and when
// the update is cancelled mid-signing (the restore runs on a context that is not done).
func TestCodesignFallbackPassesTheRequirementAndRestoresTheSearchList(t *testing.T) {
	dir, bin, req := signFixture(t, false)
	list := `    "` + loginKC + `"` + "\n" + `    "` + systemKC + `"` + "\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, tc := range []struct {
		name   string
		failIf func([]string) bool
	}{
		{"signed", nil},
		{"codesign fails", func(a []string) bool { return slices.Contains(a, "--sign") }},
		{"cancelled during codesign", func(a []string) bool {
			if slices.Contains(a, "--sign") {
				cancel()
				return true
			}
			return false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &signerExec{dr: req, list: list, failIf: tc.failIf}
			sa, err := (&macvm.HostAgentSigner{Dir: dir, Exec: fx.exec}).SignAgent(ctx, bin)
			if (err == nil) != (tc.failIf == nil) {
				t.Fatalf("err %v", err)
			}
			sign := fx.index("/usr/bin/codesign", "--force")
			if sign < 0 {
				t.Fatalf("codesign --sign never ran: %q", fx.runs)
			}
			argv := fx.runs[sign]
			if !slices.Contains(argv, "-r=designated => "+req) {
				t.Fatalf("codesign argv %q lacks the requirement element %q", argv, "-r=designated => "+req)
			}
			if n := len(slices.DeleteFunc(slices.Clone(argv), func(a string) bool { return !strings.HasPrefix(a, "-r") })); n != 1 {
				t.Fatalf("codesign argv %q has %d requirement options, want 1", argv, n)
			}
			kc := fx.runs[fx.index("/usr/bin/security", "create-keychain")][4]
			read := fx.index("/usr/bin/security", "list-keychains", "-d", "user")
			add := slices.IndexFunc(fx.runs, func(a []string) bool {
				return slices.Equal(a, []string{"/usr/bin/security", "list-keychains", "-d", "user", "-s", kc, loginKC, systemKC})
			})
			restore := slices.IndexFunc(fx.runs, func(a []string) bool {
				return slices.Equal(a, []string{"/usr/bin/security", "list-keychains", "-d", "user", "-s", loginKC, systemKC})
			})
			del := fx.index("/usr/bin/security", "delete-keychain")
			if read < 0 || !slices.Equal(fx.runs[read], []string{"/usr/bin/security", "list-keychains", "-d", "user"}) || add <= read || sign <= add ||
				restore <= sign || del <= restore {
				t.Fatalf("search list read %d, add %d, codesign %d, restore %d, delete %d:\n%q", read, add, sign, restore, del, fx.runs)
			}
			if fx.done[restore] || fx.done[del] {
				t.Fatalf("the restore (%v) or the delete (%v) ran on a done context", fx.done[restore], fx.done[del])
			}
			if err == nil && (sa.Signer != macvm.AgentSignerCodesign || len(sa.Errors) != 1 || !strings.HasPrefix(sa.Errors[0], macvm.AgentSignerWardyn+": ")) {
				t.Fatalf("signer %q errors %q, want codesign after the Wardyn signer's error", sa.Signer, sa.Errors)
			}
		})
	}
}

// Wardyn's own signer goes first, then codesign, then the build as it is (ad hoc). Each signed copy must pass
// codesign --verify --strict and read back exactly AgentRequirement before it is pushed; a copy that does not
// moves on to the next signer. The errors of each signer tried are recorded with no path.
func TestAgentSignerOrderWardynCodesignAdHoc(t *testing.T) {
	dir, bin, req := signFixture(t, true)

	// The Wardyn signer: no keychain, no openssl; only the two read-backs, on the copy.
	fx := &signerExec{dr: req}
	sa, err := (&macvm.HostAgentSigner{Dir: dir, Exec: fx.exec}).SignAgent(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	defer sa.Done()
	want := [][]string{{"/usr/bin/codesign", "--verify", "--strict", sa.Path}, {"/usr/bin/codesign", "-d", "-r-", sa.Path}}
	if sa.Signer != macvm.AgentSignerWardyn || len(sa.Errors) != 0 || !slices.EqualFunc(fx.runs, want, slices.Equal[[]string]) {
		t.Fatalf("signer %q errors %q ran %q", sa.Signer, sa.Errors, fx.runs)
	}
	b, err := os.ReadFile(sa.Path)
	if err != nil {
		t.Fatal(err)
	}
	gm, err := gomacho.NewFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	cs := gm.CodeSignature()
	if cs == nil || len(cs.CodeDirectories) != 1 || cs.CodeDirectories[0].ID != macvm.AgentSigningID || len(cs.Requirements) != 1 || machosigntest.AppleText(cs.Requirements[0].Detail) != req {
		t.Fatalf("the Wardyn-signed copy re-parses as %+v, want the designated requirement %q", cs, req)
	}
	if orig, _ := os.ReadFile(bin); !bytes.Equal(orig, machosigntest.MachO(2, 0x40)) {
		t.Fatal("the signer changed the helper's own agent binary")
	}

	// The Wardyn-signed copy fails the strict verify: codesign signs, and the record keeps why.
	var verifies atomic.Int64
	fx = &signerExec{dr: req, failIf: func(a []string) bool { return slices.Contains(a, "--verify") && verifies.Add(1) == 1 }}
	sa2, err := (&macvm.HostAgentSigner{Dir: dir, Exec: fx.exec}).SignAgent(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	defer sa2.Done()
	if sa2.Signer != macvm.AgentSignerCodesign || len(sa2.Errors) != 1 || !strings.HasPrefix(sa2.Errors[0], "wardyn: ") || fx.index("/usr/bin/security", "create-keychain") < 0 {
		t.Fatalf("signer %q errors %q ran %q", sa2.Signer, sa2.Errors, fx.runs)
	}

	// A read-back that is not exactly AgentRequirement fails every signer: an error naming each, path-free.
	fx = &signerExec{dr: `identifier "com.wardyn.guest-agent" and certificate leaf = H"00"`}
	_, err = (&macvm.HostAgentSigner{Dir: dir, Exec: fx.exec}).SignAgent(context.Background(), bin)
	var se *macvm.AgentSignError
	if !errors.As(err, &se) || len(se.Errors) != 2 || !strings.HasPrefix(se.Errors[0], "wardyn: ") || !strings.HasPrefix(se.Errors[1], "codesign: ") {
		t.Fatalf("err %v", err)
	}
	for _, e := range se.Errors {
		if strings.Contains(e, dir) || strings.Contains(e, "wardyn-agent-sign-") || strings.Contains(e, bin) || strings.Contains(e, "/") {
			t.Fatalf("a recorded error names a path: %q", e)
		}
	}

	// An identity that does not read: one error, without the signing directory's path.
	broken := filepath.Join(t.TempDir(), "agent-signing")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "cert.pem"), []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = (&macvm.HostAgentSigner{Dir: broken, Exec: fx.exec}).SignAgent(context.Background(), bin)
	if !errors.As(err, &se) || len(se.Errors) != 1 || strings.Contains(se.Errors[0], broken) {
		t.Fatalf("identity error %v", err)
	}

	// Through a tools update: the build as it is goes in, and the result and tools.json say ad_hoc and why.
	f, g := newTools(t), newGuest()
	f.opts.Log = slog.New(slog.DiscardHandler)
	f.opts.Agent = &macvm.AgentPin{Version: "0.10.1", Path: bin, Dest: guestagent.MacAgentBinaryPath}
	f.opts.Signer = &macvm.HostAgentSigner{Dir: dir, Exec: fx.exec}
	var boots, stops atomic.Int64
	res := runStage(t, f.stage(g, &boots, &stops))
	if res.AgentSignature != macvm.AgentSignedAdHoc || res.AgentSigner != macvm.AgentSignerAdHoc || len(res.AgentSignErrors) != 2 {
		t.Fatalf("signature %q signer %q errors %q", res.AgentSignature, res.AgentSigner, res.AgentSignErrors)
	}
	// And signed by Wardyn: this_mac, signer wardyn, no errors.
	f.opts.Signer = &macvm.HostAgentSigner{Dir: dir, Exec: (&signerExec{dr: req}).exec}
	res = runStage(t, f.stage(newGuest(), &boots, &stops))
	if res.AgentSignature != macvm.AgentSignedThisMac || res.AgentSigner != macvm.AgentSignerWardyn || len(res.AgentSignErrors) != 0 {
		t.Fatalf("signature %q signer %q errors %q", res.AgentSignature, res.AgentSigner, res.AgentSignErrors)
	}
	rec, err := json.Marshal(macvm.ToolsRecord{AgentSignRecord: macvm.AgentSignRecord{AgentSigner: macvm.AgentSignerAdHoc, AgentSignErrors: []string{"wardyn: x", "codesign: y"}}})
	if err != nil || !bytes.Contains(rec, []byte(`"agent_signer":"ad_hoc","agent_sign_errors":["wardyn: x","codesign: y"]`)) {
		t.Fatalf("tools.json %s (%v)", rec, err)
	}
}

// No path reaches tools.json's agent_sign_errors: not the temp directory a failed MkdirTemp names, and not a
// keychain path that security prints, in full or under "~".
func TestAgentSignErrorsArePathFree(t *testing.T) {
	dir, bin, _ := signFixture(t, false)
	home, _ := os.UserHomeDir()
	pathFree := func(t *testing.T, err error) {
		t.Helper()
		var se *macvm.AgentSignError
		if !errors.As(err, &se) || len(se.Errors) == 0 {
			t.Fatalf("err %v, want an *AgentSignError", err)
		}
		for _, e := range se.Errors {
			if strings.Contains(e, "/") || strings.Contains(e, "Keychains") || strings.Contains(e, "nowhere") {
				t.Fatalf("a recorded error names a path: %q", e)
			}
		}
	}
	keychainOut := "security: SecKeychainCreate " + home + "/Library/Keychains/signing.keychain-db: A keychain with the same name already exists.\n" +
		"search list: \"~/Library/Keychains/login.keychain-db\" (/Library/Keychains/System.keychain)"
	exec := func(_ context.Context, argv []string) ([]byte, error) {
		if slices.Contains(argv, "create-keychain") {
			return []byte(keychainOut), errors.New("exit status 48")
		}
		return nil, nil
	}
	_, err := (&macvm.HostAgentSigner{Dir: dir, Exec: exec}).SignAgent(context.Background(), bin)
	pathFree(t, err)
	if !strings.Contains(err.Error(), home+"/Library/Keychains/signing.keychain-db") {
		t.Fatalf("the log text lost the keychain path: %v", err)
	}

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "nowhere", "tmp"))
	_, err = (&macvm.HostAgentSigner{Dir: dir, Exec: exec}).SignAgent(context.Background(), bin)
	pathFree(t, err)
}

// The read-back's identifier must match exactly, as macOS compares it; only the certificate hash's hex may
// differ in case.
func TestRequirementReadBackComparesTheIdentifierExactly(t *testing.T) {
	dir, bin, req := signFixture(t, true)
	pre, hash, _ := strings.Cut(req, `H"`)
	upperHex := pre + `H"` + strings.ToUpper(hash)
	sa, err := (&macvm.HostAgentSigner{Dir: dir, Exec: (&signerExec{dr: upperHex}).exec}).SignAgent(context.Background(), bin)
	if err != nil || sa.Signer != macvm.AgentSignerWardyn {
		t.Fatalf("upper-case hex: signer %q err %v", sa.Signer, err)
	}
	sa.Done()
	wrongCase := strings.Replace(req, macvm.AgentSigningID, strings.ToUpper(macvm.AgentSigningID), 1)
	if _, err := (&macvm.HostAgentSigner{Dir: dir, Exec: (&signerExec{dr: wrongCase}).exec}).SignAgent(context.Background(), bin); err == nil {
		t.Fatalf("a read-back with the identifier %q in another case passed", strings.ToUpper(macvm.AgentSigningID))
	}
}
