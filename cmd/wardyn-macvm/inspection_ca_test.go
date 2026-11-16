// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// --inspection-ca adds this Mac's CA to stage 2's install list, after the operator's items, and a file that is
// not a CA stops the job before any boot.
func TestStage2AddsTheInspectionCA(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	listPath := filepath.Join(dir, "items.json")
	notCA := filepath.Join(dir, "not-a-ca.pem")
	_ = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(listPath, []byte(`{"version":1,"items":[{"kind":"pkg","name":"edr","path":"/opt/edr.pkg","sha256":"`+strings.Repeat("a", 64)+`"}]}`), 0o600)
	_ = os.WriteFile(notCA, []byte("text"), 0o600)
	for _, tc := range []struct {
		name string
		args []string
		want int // items; -1 for an error
	}{
		{"with the list", []string{"--" + macvm.FlagBaseFile, listPath, "--" + macvm.FlagInspectionCA, caPath}, 2},
		{"alone", []string{"--" + macvm.FlagInspectionCA, caPath}, 1},
		{"not a CA", []string{"--" + macvm.FlagInspectionCA, notCA}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			j := newJobFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			st, err := j.stage2(nil, deps{}, nil, "/agent")
			if tc.want < 0 {
				if err == nil {
					t.Error("accepted")
				}
				return
			}
			if err != nil || len(st.Items.Items) != tc.want {
				t.Fatalf("%v %+v", err, st)
			}
			last := st.Items.Items[tc.want-1]
			if last.Name != macvm.InspectionCAItem || last.Path != caPath {
				t.Errorf("last item %+v", last)
			}
		})
	}
}
