// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// ebwGuest is the edit's guest whose verify-cert answers by certificate name (an exit code, 1 by default is
// not trusted) and whose posture says fda for Full Disk Access.
type ebwGuest struct {
	*editGuest
	trusted map[string]bool
	fda     string
}

func (g *ebwGuest) Posture(ctx context.Context) (guestagent.ToolsPosture, error) {
	p, err := g.editGuest.Posture(ctx)
	p.FullDiskAccess = g.fda
	return p, err
}

func (g *ebwGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if len(argv) > 1 && argv[1] == "verify-cert" {
		name := filepath.Base(filepath.Dir(argv[len(argv)-1]))
		if g.trusted[name] {
			return "", 0, nil
		}
		return "", 1, nil
	}
	return g.editGuest.Run(ctx, argv)
}

// ebwEdit is an edit of a base whose tools.json holds two ca items, acme-root and acme-proxy, both read as
// not trusted in the record; the install list names acme-proxy's real certificate file. It returns the
// guest and the certificate's DER.
func ebwEdit(t *testing.T) (*editFix, *ebwGuest, []byte) {
	t.Helper()
	e := newEdit(t)
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rootPath, rootPin, rootDER := caPEM(t, dir, "acme-root", true)
	proxyPath, proxyPin, proxyDER := caPEM(t, dir, "acme-proxy", true)
	fp := func(der []byte) string { h := sha256.Sum256(der); return hex.EncodeToString(h[:]) }
	e.opts.Items = macvm.BaseList{Version: 1, Items: []macvm.BaseItem{
		{Kind: macvm.ItemKindCA, Name: "acme-root", Path: rootPath, SHA256: rootPin},
		{Kind: macvm.ItemKindCA, Name: "acme-proxy", Path: proxyPath, SHA256: proxyPin}}}
	rec.Guest, rec.Items = nil, []macvm.ItemRecord{
		{Kind: macvm.ItemKindCA, Name: "acme-root", SHA256: rootPin, State: macvm.ItemTrusted, Fingerprint: fp(rootDER)},
		{Kind: macvm.ItemKindCA, Name: "acme-proxy", SHA256: proxyPin, State: macvm.ItemNotTrusted, Fingerprint: fp(proxyDER)},
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(e.base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	e.g.control = false
	g := &ebwGuest{editGuest: e.g, trusted: map[string]bool{"acme-root": true}, fda: "allowed"}
	e.opts.Guest = func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		e.boots++
		return g, func(context.Context) error { return nil }, nil
	}
	return e, g, proxyDER
}

// squash reads a printed row as its words, so the column padding is not the test's business.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// edit-base prints, once and before the window's wait, each ca item the base reads as not trusted, with its
// subject, SHA-256 and the exact command; a trusted item prints nothing, and with none untrusted there is no
// header at all.
func TestEditBasePrintsOnlyTheUntrustedCertificates(t *testing.T) {
	e, _, der := ebwEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(der)
	var colon []string
	for _, b := range h {
		colon = append(colon, strings.ToUpper(hex.EncodeToString([]byte{b})))
	}
	out := e.out.String()
	for _, want := range []string{
		"Not yet trusted in this base. Click Trust the list's CAs in the window, or run each command in the admin Terminal. Check that the subject and SHA-256 are the ones you expect, then confirm macOS's prompt.\nacme-proxy\n",
		"  subject  \"CN=acme-proxy\"\n",
		"  SHA-256  " + strings.Join(colon, ":") + "\n",
		"  sudo launchctl asuser $(stat -f %u /dev/console) security add-trusted-cert -d -r trustRoot -p ssl -k /Library/Keychains/System.keychain /Library/Wardyn/stage/acme-proxy/item.cer\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	before, _, _ := strings.Cut(out, macvm.EditCheckingLine)
	if strings.Count(before, "Not yet trusted") != 1 || strings.Contains(before, "acme-root") {
		t.Errorf("before the window's wait:\n%s", before)
	}

	// Every certificate already trusted: no block.
	e, _, _ = ebwEdit(t)
	rec, _ := macvm.ReadToolsRecord(e.base)
	rec.Items[1].State = macvm.ItemTrusted
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(e.base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if out := e.out.String(); strings.Contains(out, "Not yet trusted") || strings.Contains(out, "add-trusted-cert") {
		t.Errorf("a certificate block with nothing to trust:\n%s", out)
	}
}

// An install list whose file is not the one the base was built from gives no subject, and the base's own
// recorded fingerprint stands in for the hash.
func TestEditBaseCertificateBlockIgnoresAListFileThatChanged(t *testing.T) {
	e, _, _ := ebwEdit(t)
	e.opts.Items.Items[1].SHA256 = strings.Repeat("e", 64)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if strings.Contains(out, "acme-proxy\"") || !strings.Contains(out, "  subject  not available: the install list's file is not the one this base was built from\n") {
		t.Errorf("the subject came from a file the base was not built from:\n%s", out)
	}
	rec, _ := macvm.ReadToolsRecord(e.base)
	if !strings.Contains(out, "  SHA-256  "+strings.ToUpper(rec.Items[1].Fingerprint[:2])+":") {
		t.Errorf("the recorded fingerprint was not used:\n%s", out)
	}
}

// The recheck prints what the guest read in that boot, and records it with the time: each certificate, Full
// Disk Access as the agent answered, and the desktop in words. An answer that is neither allowed nor refused
// reads "could not be read" and is recorded empty, never as allowed.
func TestEditBaseReportsWhatTheBootRead(t *testing.T) {
	for _, tc := range []struct{ fda, row, kept string }{
		{"allowed", "Full Disk Access allowed", "allowed"},
		{"refused", "Full Disk Access refused", "refused"},
		{"", "Full Disk Access could not be read", ""},
		{"maybe", "Full Disk Access could not be read", ""},
	} {
		t.Run(tc.fda, func(t *testing.T) {
			e, g, _ := ebwEdit(t)
			g.fda = tc.fda
			if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
				t.Fatal(err)
			}
			var rows []string
			for _, l := range e.lines() {
				if strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "  subject") && !strings.HasPrefix(l, "  SHA-256") && !strings.HasPrefix(l, "  sudo") {
					rows = append(rows, squash(l))
				}
			}
			want := []string{"certificate acme-root trusted", "certificate acme-proxy not trusted", tc.row, "desktop view only"}
			if !slices.Equal(rows, want) {
				t.Fatalf("rows %q, want %q\n%s", rows, want, e.out.String())
			}
			if !slices.Contains(e.lines(), "Read from the edited base in this boot:") || !slices.Contains(e.lines(), "The edited base is ready, and runs now use it. Desktop: view only.") {
				t.Errorf("lines:\n%s", e.out.String())
			}
			m, _ := macvm.ReadyBase(e.base)
			rec, ok := macvm.ReadEditRecord(e.base, m)
			wantCerts := map[string]string{"acme-root": macvm.ItemTrusted, "acme-proxy": macvm.ItemNotTrusted}
			if !ok || !rec.ReadAt.Equal(e.opts.Now()) || rec.FullDiskAccess != tc.kept || len(rec.Certs) != 2 || rec.Certs["acme-root"] != wantCerts["acme-root"] || rec.Certs["acme-proxy"] != wantCerts["acme-proxy"] {
				t.Errorf("record %+v %v", rec, ok)
			}
		})
	}
}

// A discarded edit keeps the last passing edit's read and its time: the status says "at the last edit".
func TestEditBaseDiscardKeepsTheLastRead(t *testing.T) {
	e, g, _ := ebwEdit(t)
	g.fda = "refused"
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	m, _ := macvm.ReadyBase(e.base)
	first, _ := macvm.ReadEditRecord(e.base, m)
	e.again(t)
	g.fda = "allowed"
	g.posture.RemoteLogin = true
	if _, err := macvm.EditBase(context.Background(), e.opts); err == nil {
		t.Fatal("an edit with Remote Login on passed")
	}
	got, _ := macvm.ReadEditRecord(e.base, m)
	if got.Recheck != macvm.EditRecheckRemoteLogin || got.FullDiskAccess != "refused" || !got.ReadAt.Equal(first.ReadAt) || got.Certs["acme-proxy"] != macvm.ItemNotTrusted {
		t.Errorf("record %+v, want the first edit's read under the second's recheck", got)
	}
}

// A certificate's subject can carry a newline or an escape sequence, which the person's Terminal would act
// on: the block prints it quoted, with both escaped, on one line.
func TestEditBaseCertificateBlockEscapesTheSubject(t *testing.T) {
	e, _, _ := ebwEdit(t)
	cn := "Evil\nSHA-256  00:11\x1b[31m"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path, pin := itemFile(t, t.TempDir(), "evil.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	e.opts.Items.Items[1].Path, e.opts.Items.Items[1].SHA256 = path, pin
	rec, _ := macvm.ReadToolsRecord(e.base)
	rec.Items[1].SHA256 = pin
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(e.base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if strings.Contains(out, "\x1b") {
		t.Errorf("an escape byte reached the output: %q", out)
	}
	var subjects, hashes int
	for _, l := range e.lines() {
		subjects += strings.Count(l, "  subject  ")
		if strings.HasPrefix(l, "  SHA-256  ") {
			hashes++
		}
	}
	if subjects != 1 || hashes != 1 || !strings.Contains(out, `  subject  "CN=Evil\nSHA-256  00:11\x1b[31m"`) {
		t.Errorf("the subject was not printed escaped on one line:\n%s", out)
	}
}
