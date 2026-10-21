// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func testCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cluster-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestComponentCLI_KubectlShape: a kubectl row names an https API server with
// no path, an optional CA bundle of certificates only, one service account,
// 1..16 distinct namespaces and read or write.
func TestComponentCLI_KubectlShape(t *testing.T) {
	t.Setenv(types.EnvKubectlCLI, "on")
	ca := testCAPEM(t)
	base := types.CLIKubectl{Server: "https://k8s.example.com:6443", CAPEM: ca,
		ServiceAccount: types.CLIServiceAccount{Namespace: "wardyn", Name: "runs"}, Namespaces: []string{"team-a"}, Access: "read"}
	row := func(f func(*types.CLIKubectl)) types.ComponentDefinition {
		k := base
		f(&k)
		return cliDef(&types.ComponentCLI{Kind: "kubectl", Kubectl: &k})
	}
	tooMany := make([]string, types.MaxCLIKubectlNamespaces+1)
	for i := range tooMany {
		tooMany[i] = "ns" + string(rune('a'+i))
	}
	cases := map[string]struct {
		def  types.ComponentDefinition
		want string
	}{
		"ok":                {row(func(*types.CLIKubectl) {}), ""},
		"ok ip no ca":       {row(func(k *types.CLIKubectl) { k.Server, k.CAPEM = "https://10.0.0.1", "" }), ""},
		"ok trailing slash": {row(func(k *types.CLIKubectl) { k.Server = "https://k8s.example.com/" }), ""},
		"arm missing":       {cliDef(&types.ComponentCLI{Kind: "kubectl"}), "cli.kubectl: required"},
		"other arm":         {cliDef(&types.ComponentCLI{Kind: "kubectl", Kubectl: &base, Azure: &types.CLIAzure{}}), "cli.azure: not allowed"},
		"http":              {row(func(k *types.CLIKubectl) { k.Server = "http://k8s.example.com" }), "cli.kubectl.server"},
		"path":              {row(func(k *types.CLIKubectl) { k.Server = "https://k8s.example.com/api" }), "cli.kubectl.server"},
		"userinfo":          {row(func(k *types.CLIKubectl) { k.Server = "https://u:p@k8s.example.com" }), "cli.kubectl.server"},
		"query":             {row(func(k *types.CLIKubectl) { k.Server = "https://k8s.example.com?x=1" }), "cli.kubectl.server"},
		"bad port":          {row(func(k *types.CLIKubectl) { k.Server = "https://k8s.example.com:99999" }), "port"},
		"bad host":          {row(func(k *types.CLIKubectl) { k.Server = "https://k8s_example.com" }), "not a host name"},
		"ca not pem":        {row(func(k *types.CLIKubectl) { k.CAPEM = "nope" }), "ca_pem"},
		"ca key block": {row(func(k *types.CLIKubectl) {
			k.CAPEM = ca + "-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n"
		}), "ca_pem"},
		"ca trailing junk": {row(func(k *types.CLIKubectl) { k.CAPEM = ca + "junk" }), "ca_pem"},
		"sa namespace":     {row(func(k *types.CLIKubectl) { k.ServiceAccount.Namespace = "Bad" }), "service_account.namespace"},
		"sa name":          {row(func(k *types.CLIKubectl) { k.ServiceAccount.Name = "" }), "service_account.name"},
		"no namespaces":    {row(func(k *types.CLIKubectl) { k.Namespaces = nil }), "1 to 16"},
		"too many":         {row(func(k *types.CLIKubectl) { k.Namespaces = tooMany }), "1 to 16"},
		"bad namespace":    {row(func(k *types.CLIKubectl) { k.Namespaces = []string{"a/b"} }), "namespaces[0]"},
		"twice":            {row(func(k *types.CLIKubectl) { k.Namespaces = []string{"a", "a"} }), "listed twice"},
		"access":           {row(func(k *types.CLIKubectl) { k.Access = "admin" }), "read or write"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := c.def.Validate(proxy.ValidDomainEntry, false)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
	host, port, ok := base.HostPort()
	if !ok || host != "k8s.example.com" || port != 6443 {
		t.Errorf("HostPort = %q %d %v", host, port, ok)
	}
	if _, port, _ := (types.CLIKubectl{Server: "https://K8S.example.com"}).HostPort(); port != 443 {
		t.Errorf("default port = %d, want 443", port)
	}
}

// TestComponentCLI_KubectlOffIsToday: with the switch off (the default) a
// kubectl row, however well formed, is refused exactly as an unknown kind was
// before kubectl existed.
func TestComponentCLI_KubectlOffIsToday(t *testing.T) {
	t.Setenv(types.EnvKubectlCLI, "")
	k := types.CLIKubectl{Server: "https://k8s.example.com", ServiceAccount: types.CLIServiceAccount{Namespace: "w", Name: "r"},
		Namespaces: []string{"a"}, Access: "read"}
	err := cliDef(&types.ComponentCLI{Kind: "kubectl", Kubectl: &k}).Validate(proxy.ValidDomainEntry, false)
	if err == nil || err.Error() != `cli.kind: "kubectl" is not azure, aws or github` {
		t.Fatalf("switch off: %v, want today's unknown-kind refusal", err)
	}
	t.Setenv(types.EnvKubectlCLI, "1")
	if err := cliDef(&types.ComponentCLI{Kind: "kubectl", Kubectl: &k}).Validate(proxy.ValidDomainEntry, false); err != nil {
		t.Fatalf("switch on: %v", err)
	}
}
