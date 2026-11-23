// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/devicecert"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type fakeSource struct {
	ids []deviceIdentity
	err error
}

func (f fakeSource) Identities() ([]deviceIdentity, error) { return f.ids, f.err }

var identityNow = time.Unix(1795600000, 0).UTC()

// identityFixture is a runner state directory with a real runner key, and the fingerprint it names.
func identityFixture(t *testing.T) (dir, fingerprint string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "runner.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, runnerwire.Fingerprint(pub)
}

func deviceDeps(src identitySource) (deps, *bytes.Buffer, *bytes.Buffer) {
	d, out, errb := testDeps(&macvmtest.Engine{})
	d.identities, d.now = src, func() time.Time { return identityNow }
	return d, out, errb
}

func orgHash() string {
	sum := sha256.Sum256([]byte("https://org.example"))
	return hex.EncodeToString(sum[:])
}

// A proof is a signature over the message the helper built from the key file's fingerprint: an identity whose
// signature checks under its own public key over exactly devicecert.Message proves the helper chose the bytes.
func TestDeviceIdentity_SignsOnlyTheDeviceMessage(t *testing.T) {
	dir, fp := identityFixture(t)
	dev, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var signed [][]byte
	id := deviceIdentity{Chain: [][]byte{{1, 2, 3}, {4}}, sign: func(msg []byte) ([]byte, error) {
		signed = append(signed, msg)
		h := sha256.Sum256(msg)
		return ecdsa.SignASN1(rand.Reader, dev, h[:])
	}}
	d, out, errb := deviceDeps(fakeSource{ids: []deviceIdentity{id}})
	args := []string{"device-identity", "--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", identityNow.Format(time.RFC3339)}
	if code := run(context.Background(), args, d); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	want, err := devicecert.Message(fp, orgHash(), identityNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(signed) != 1 || !bytes.Equal(signed[0], want) || !strings.HasPrefix(string(signed[0]), devicecert.Prefix) {
		t.Fatalf("signed %q, want %q", signed, want)
	}
	var rep runnerwire.DeviceCertReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || len(rep.Proofs) != 1 {
		t.Fatalf("output %q: %v", out, err)
	}
	p := rep.Proofs[0]
	h := sha256.Sum256(want)
	if !ecdsa.VerifyASN1(&dev.PublicKey, h[:], p.Signature) || !p.SignedAt.Equal(identityNow) || len(p.Chain) != 2 {
		t.Fatalf("proof %+v does not verify", p)
	}
}

func TestDeviceIdentity_RefusesBadFlags(t *testing.T) {
	dir, _ := identityFixture(t)
	emptyDir := t.TempDir()
	badMode, badSize := t.TempDir(), t.TempDir()
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	if err := os.WriteFile(filepath.Join(badMode, "runner.key"), k, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(badMode, "runner.key"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badSize, "runner.key"), k[:40], 0600); err != nil {
		t.Fatal(err)
	}
	at := identityNow.Format(time.RFC3339)
	for name, args := range map[string][]string{
		"no state dir":                 {"--org-sha256", orgHash(), "--signed-at", at},
		"a key file absent":            {"--state-dir", emptyDir, "--org-sha256", orgHash(), "--signed-at", at},
		"a short org hash":             {"--state-dir", dir, "--org-sha256", "abc", "--signed-at", at},
		"an upper-case org":            {"--state-dir", dir, "--org-sha256", strings.ToUpper(orgHash()), "--signed-at", at},
		"a bad time":                   {"--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", "yesterday"},
		"a time too early":             {"--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", identityNow.Add(-devicecert.MaxSkew - time.Minute).Format(time.RFC3339)},
		"a time too late":              {"--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", identityNow.Add(devicecert.MaxSkew + time.Minute).Format(time.RFC3339)},
		"a fingerprint flag":           {"--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", at, "--fingerprint", strings.Repeat("a", 64)},
		"a message flag":               {"--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", at, "--message", "x"},
		"a key file that is not 0600":  {"--state-dir", badMode, "--org-sha256", orgHash(), "--signed-at", at},
		"a key file of the wrong size": {"--state-dir", badSize, "--org-sha256", orgHash(), "--signed-at", at},
		"a stray argument":             {"--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", at, "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			signed := false
			id := deviceIdentity{Chain: [][]byte{{1}}, sign: func([]byte) ([]byte, error) { signed = true; return []byte{1}, nil }}
			d, out, errb := deviceDeps(fakeSource{ids: []deviceIdentity{id}})
			code := run(context.Background(), append([]string{"device-identity"}, args...), d)
			if code != 2 || signed || out.Len() != 0 || !strings.HasPrefix(errb.String(), "wardyn-macvm: ") {
				t.Fatalf("exit %d, signed %v, stdout %q, stderr %q", code, signed, out, errb)
			}
		})
	}
}

func TestDeviceIdentity_KeepsToTheBounds(t *testing.T) {
	dir, _ := identityFixture(t)
	args := []string{"device-identity", "--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", identityNow.Format(time.RFC3339)}
	sig := func([]byte) ([]byte, error) { return []byte{7}, nil }
	var ids []deviceIdentity
	for i := 0; i < devicecert.MaxProofs+3; i++ {
		ids = append(ids, deviceIdentity{Chain: [][]byte{{byte(i + 1)}}, sign: sig})
	}
	ids = append(ids,
		deviceIdentity{Chain: nil, sign: sig},
		deviceIdentity{Chain: make([][]byte, devicecert.MaxChainLen+1), sign: sig},
		deviceIdentity{Chain: [][]byte{make([]byte, devicecert.MaxDER+1)}, sign: sig},
		deviceIdentity{Chain: [][]byte{{1}}, sign: func([]byte) ([]byte, error) { return nil, errKeyType }})
	d, out, errb := deviceDeps(fakeSource{ids: ids})
	if code := run(context.Background(), args, d); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var rep runnerwire.DeviceCertReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || len(rep.Proofs) != devicecert.MaxProofs {
		t.Fatalf("%d proofs (%v), want %d", len(rep.Proofs), err, devicecert.MaxProofs)
	}
}

func TestDeviceIdentity_AKeyOfAnotherTypeIsSkippedAndARefusalIsSkippedOrFailsWhenAlone(t *testing.T) {
	dir, _ := identityFixture(t)
	args := []string{"device-identity", "--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", identityNow.Format(time.RFC3339)}
	d, out, _ := deviceDeps(fakeSource{ids: []deviceIdentity{{Chain: [][]byte{{1}}, sign: func([]byte) ([]byte, error) { return nil, errKeyType }}}})
	if code := run(context.Background(), args, d); code != 0 || strings.TrimSpace(out.String()) != `{"proofs":[]}` {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	// Every identity refuses: nothing could be signed, exit 2.
	denied := func([]byte) ([]byte, error) { return nil, errors.New("denied") }
	d, out, errb := deviceDeps(fakeSource{ids: []deviceIdentity{{Chain: [][]byte{{1}}, sign: denied}}})
	if code := run(context.Background(), args, d); code != 2 || out.Len() != 0 || !strings.Contains(errb.String(), "denied") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
	// One refuses and one signs: the signing one is reported, and the refusal is named by its certificate's hash.
	sum := sha256.Sum256([]byte{1})
	ok := func([]byte) ([]byte, error) { return []byte{7}, nil }
	d, out, errb = deviceDeps(fakeSource{ids: []deviceIdentity{{Chain: [][]byte{{1}}, sign: denied}, {Chain: [][]byte{{2}}, sign: ok}}})
	var rep runnerwire.DeviceCertReport
	if code := run(context.Background(), args, d); code != 0 || json.Unmarshal(out.Bytes(), &rep) != nil || len(rep.Proofs) != 1 || rep.Proofs[0].Chain[0][0] != 2 ||
		strings.Count(errb.String(), "\n") != 1 || !strings.Contains(errb.String(), hex.EncodeToString(sum[:])) || !strings.Contains(errb.String(), "denied") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
	d, out, errb = deviceDeps(fakeSource{err: errors.New("no keychain")})
	if code := run(context.Background(), args, d); code != 2 || !strings.Contains(errb.String(), "no keychain") {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
}

func TestDeviceIdentity_OtherBuildsReadNothing(t *testing.T) {
	if vzLinked {
		t.Skip("this build reads the keychain")
	}
	if _, err := (keychainIdentities{}).Identities(); err == nil {
		t.Fatal("want an error off a Mac")
	}
}

// The fingerprint in the message is the one of the key file in --state-dir: two runners' directories give two
// messages, and no flag names a fingerprint.
func TestDeviceIdentity_FingerprintFromLocalKey(t *testing.T) {
	args := func(dir string) []string {
		return []string{"device-identity", "--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", identityNow.Format(time.RFC3339)}
	}
	var msgs []string
	for i := 0; i < 2; i++ {
		dir, fp := identityFixture(t)
		var signed []byte
		id := deviceIdentity{Chain: [][]byte{{1}}, sign: func(m []byte) ([]byte, error) { signed = m; return []byte{1}, nil }}
		d, _, errb := deviceDeps(fakeSource{ids: []deviceIdentity{id}})
		if code := run(context.Background(), args(dir), d); code != 0 {
			t.Fatalf("exit %d: %s", code, errb)
		}
		if !strings.Contains(string(signed), "\n"+fp+"\n") {
			t.Fatalf("message %q does not name the key file's fingerprint %s", signed, fp)
		}
		msgs = append(msgs, string(signed))
	}
	if msgs[0] == msgs[1] {
		t.Fatal("two runners signed the same message")
	}
}

// The cap of MaxProofs counts proofs: identities that cannot be reported come first in the keychain and must not
// use the places.
func TestDeviceIdentity_TheCapCountsProofsNotIdentities(t *testing.T) {
	dir, _ := identityFixture(t)
	args := []string{"device-identity", "--state-dir", dir, "--org-sha256", orgHash(), "--signed-at", identityNow.Format(time.RFC3339)}
	var ids []deviceIdentity
	for i := 0; i < devicecert.MaxProofs; i++ {
		switch i % 3 {
		case 0:
			ids = append(ids, deviceIdentity{Chain: nil})
		case 1:
			ids = append(ids, deviceIdentity{Chain: [][]byte{{1}}, sign: func([]byte) ([]byte, error) { return nil, errKeyType }})
		default:
			ids = append(ids, deviceIdentity{Chain: [][]byte{{1}}, sign: func([]byte) ([]byte, error) { return nil, errors.New("denied") }})
		}
	}
	ids = append(ids, deviceIdentity{Chain: [][]byte{{9}}, sign: func([]byte) ([]byte, error) { return []byte{7}, nil }})
	d, out, errb := deviceDeps(fakeSource{ids: ids})
	var rep runnerwire.DeviceCertReport
	if code := run(context.Background(), args, d); code != 0 || json.Unmarshal(out.Bytes(), &rep) != nil || len(rep.Proofs) != 1 || rep.Proofs[0].Chain[0][0] != 9 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
}
