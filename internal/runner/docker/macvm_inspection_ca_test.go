// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// inspectionRunCA is a run CA as the dispatcher makes one: a CA that signs only leaves, valid one year, with
// its key as PKCS#8 PEM.
func inspectionRunCA(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Wardyn run egress-inspection CA", Organization: []string{"Wardyn"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		SubjectKeyId:          []byte("run-ca-key-id"),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

// inspectionSpec is macSpec with a run CA and a host the proxy terminates. The proxy terminates nothing
// without the key.
func inspectionSpec(t *testing.T) runner.SandboxSpec {
	t.Helper()
	s := macSpec()
	s.ProxyConfig.MITMCACertPEM, s.ProxyConfig.MITMCAKeyPEM = inspectionRunCA(t)
	s.ProxyConfig.MITMHosts = []string{"api.example.com:443"}
	return s
}

// inspectionConfig is a config with a Mac CA, and a Standard base whose tools record holds that CA in state
// (no record when state is "").
func inspectionConfig(t *testing.T, state string) (*MacVMConfig, *macvm.InspectionCA) {
	t.Helper()
	l := macvm.Layout{Root: t.TempDir()}
	ca, err := macvm.OpenInspectionCA(context.Background(), l.InspectionCADir())
	if err != nil {
		t.Fatal(err)
	}
	base := l.BaseDir(0)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		writeInspectionTools(t, base, ca, state)
	}
	return &MacVMConfig{Layout: l, InspectionCA: ca}, ca
}

func writeInspectionTools(t *testing.T, dir string, ca *macvm.InspectionCA, state string) {
	t.Helper()
	b, err := json.Marshal(macvm.ToolsRecord{Items: []macvm.ItemRecord{{Kind: macvm.ItemKindCA, Name: macvm.InspectionCAItem, State: state, Fingerprint: ca.Fingerprint()}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func parseInspectionCert(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		t.Fatal("not a PEM certificate")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMacVMInspectionOffChangesNothing(t *testing.T) {
	trusted, _ := inspectionConfig(t, macvm.ItemTrusted)
	noCert := inspectionSpec(t)
	noCert.ProxyConfig.MITMCACertPEM = ""
	noHosts := inspectionSpec(t)
	noHosts.ProxyConfig.MITMHosts = nil
	for name, c := range map[string]struct {
		cfg  *MacVMConfig
		spec runner.SandboxSpec
	}{
		"no CA":                     {&MacVMConfig{Layout: macvm.Layout{Root: t.TempDir()}}, inspectionSpec(t)},
		"the proxy has no CA":       {trusted, noCert},
		"the proxy terminates none": {trusted, noHosts},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := c.cfg.macVMInspection(c.spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.proxy.ProxyConfig.MITMCACertPEM != c.spec.ProxyConfig.MITMCACertPEM {
				t.Error("the proxy's CA changed")
			}
			if got.signed || got.state != "" || got.sha256 != "" {
				t.Errorf("recorded %v %q %q", got.signed, got.state, got.sha256)
			}
		})
	}
}

func TestMacVMInspectionUntrustedBaseKeepsTheRunCA(t *testing.T) {
	for _, state := range []string{"", macvm.ItemNotTrusted} {
		t.Run("state "+state, func(t *testing.T) {
			c, ca := inspectionConfig(t, state)
			spec := inspectionSpec(t)
			got, err := c.macVMInspection(spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.proxy.ProxyConfig.MITMCACertPEM != spec.ProxyConfig.MITMCACertPEM {
				t.Error("the proxy's CA changed")
			}
			if got.signed || got.state != types.InspectionCANotTrusted || got.sha256 != ca.Fingerprint() {
				t.Errorf("got %v %q %q", got.signed, got.state, got.sha256)
			}
		})
	}
}

func TestMacVMInspectionTrustedBaseCrossSignsForTheProxyOnly(t *testing.T) {
	c, ca := inspectionConfig(t, macvm.ItemTrusted)
	spec := inspectionSpec(t)
	runPEM := spec.ProxyConfig.MITMCACertPEM
	got, err := c.macVMInspection(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.signed || got.state != types.InspectionCATrusted || got.sha256 != ca.Fingerprint() {
		t.Fatalf("got %v %q %q", got.signed, got.state, got.sha256)
	}
	if spec.ProxyConfig.MITMCACertPEM != runPEM {
		t.Error("the input spec's CA changed")
	}
	cross := parseInspectionCert(t, got.proxy.ProxyConfig.MITMCACertPEM)
	run := parseInspectionCert(t, runPEM)
	if string(cross.RawSubject) != string(run.RawSubject) {
		t.Error("the copy's subject is not the run CA's")
	}
	if err := cross.CheckSignatureFrom(parseInspectionCert(t, ca.PEM())); err != nil {
		t.Errorf("not signed by the Mac CA: %v", err)
	}
	if !reflect.DeepEqual(cross.PermittedDNSDomains, []string{"api.example.com"}) {
		t.Errorf("permitted: %q", cross.PermittedDNSDomains)
	}
	if cross.NotAfter.After(time.Now().Add(7*24*time.Hour + time.Minute)) {
		t.Errorf("the copy lasts to %s", cross.NotAfter)
	}
	in, _ := macGuestInit(spec, "operator-ca")
	signed := withMacInspection(in, true)
	if signed.RunCA != "" {
		t.Error("a signed run still asks the guest to trust the run CA")
	}
	if !reflect.DeepEqual(signed.Files, in.Files) {
		t.Error("the CA bundle changed")
	}
	if unsigned := withMacInspection(in, false); unsigned.RunCA != in.RunCA {
		t.Error("an unsigned run lost its run CA")
	}
}

func TestMacVMInspectionHoldsToTheRunsLifetimeCap(t *testing.T) {
	c, _ := inspectionConfig(t, macvm.ItemTrusted)
	spec := inspectionSpec(t)
	spec.Resources.LifetimeCap = 2 * time.Hour
	got, err := c.macVMInspection(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.signed {
		t.Fatal("not signed")
	}
	cross := parseInspectionCert(t, got.proxy.ProxyConfig.MITMCACertPEM)
	if cross.NotAfter.After(time.Now().Add(2*time.Hour + time.Minute)) {
		t.Errorf("the copy lasts to %s", cross.NotAfter)
	}
}

func TestMacVMInspectionResumedKeptMachineReadsItsOwnRecord(t *testing.T) {
	c, _ := inspectionConfig(t, macvm.ItemTrusted)
	kept := &macvm.KeptRun{ID: uuid.NewString(), Mode: macvm.KeptResume}
	if err := os.MkdirAll(c.Layout.KeptMachineDir(kept.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := c.macVMInspection(inspectionSpec(t), kept)
	if err != nil {
		t.Fatal(err)
	}
	if got.state != types.InspectionCANotTrusted || got.signed {
		t.Errorf("got %v %q", got.signed, got.state)
	}
}

func TestMacVMInspectionExpiredCASignsNothing(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	dir := l.InspectionCADir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "expired test CA"},
		NotBefore:             time.Now().Add(-2 * time.Hour),
		NotAfter:              time.Now().Add(-time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            1,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for f, c := range map[string]struct {
		b    []byte
		mode os.FileMode
	}{
		"key.pem": {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600},
		"ca.pem":  {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644},
	} {
		p := filepath.Join(dir, f)
		if err := os.WriteFile(p, c.b, c.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, c.mode); err != nil { // WriteFile's mode is cut by the umask
			t.Fatal(err)
		}
	}
	ca, err := macvm.OpenInspectionCA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.BaseDir(0), 0o700); err != nil {
		t.Fatal(err)
	}
	writeInspectionTools(t, l.BaseDir(0), ca, macvm.ItemTrusted)
	c := &MacVMConfig{Layout: l, InspectionCA: ca}
	spec := inspectionSpec(t)
	got, err := c.macVMInspection(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.state != types.InspectionCAExpired || got.signed || got.proxy.ProxyConfig.MITMCACertPEM != spec.ProxyConfig.MITMCACertPEM {
		t.Errorf("got %v %q", got.signed, got.state)
	}
}
