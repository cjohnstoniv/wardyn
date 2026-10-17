// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const chainCAName = "gw-root-ca.corp.internal"

// chainCert signs a certificate for tmpl with parent (self-signed when parent is
// nil) and returns it with its key.
func chainCert(t *testing.T, tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func chainCATemplate(cn string) *x509.Certificate {
	return &x509.Certificate{
		Subject: pkix.Name{CommonName: cn}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, SubjectKeyId: []byte{1, 2, 3},
	}
}

// unknownAuthorityWithHint is the error Verify returns when a root with the
// issuer's name exists but did not sign the leaf: the one shape that prints a
// CA name.
func unknownAuthorityWithHint(t *testing.T) error {
	t.Helper()
	trusted, _ := chainCert(t, chainCATemplate(chainCAName), nil, nil)
	impostor, impostorKey := chainCert(t, chainCATemplate(chainCAName), nil, nil)
	leaf, _ := chainCert(t, &x509.Certificate{Subject: pkix.Name{CommonName: "leaf"}, DNSNames: []string{"leaf.example"}}, impostor, impostorKey)
	pool := x509.NewCertPool()
	pool.AddCert(trusted)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool})
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) || !strings.Contains(err.Error(), chainCAName) {
		t.Fatalf("fixture: want an unknown-authority error naming %q, got %v", chainCAName, err)
	}
	return err
}

// nameConstrained is Verify's refusal of a leaf name the root does not permit.
func nameConstrained(t *testing.T) error {
	t.Helper()
	tmpl := chainCATemplate("constraint-root")
	tmpl.PermittedDNSDomains, tmpl.PermittedDNSDomainsCritical = []string{"permitted.corp.internal"}, true
	root, rootKey := chainCert(t, tmpl, nil, nil)
	leaf, _ := chainCert(t, &x509.Certificate{Subject: pkix.Name{CommonName: "leaf"}, DNSNames: []string{"gw.secret.internal"}}, root, rootKey)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool})
	var ci x509.CertificateInvalidError
	if !errors.As(err, &ci) || ci.Reason != x509.CANotAuthorizedForThisName || !strings.Contains(err.Error(), "gw.secret.internal") {
		t.Fatalf("fixture: want a name-constraint refusal naming the leaf, got %v", err)
	}
	return err
}

// A certificate failure carries no Host (unknown authority) or names the
// leaf and the constraint (name constraints), so no name off the chain reaches
// the sandbox or the decision row, for a gateway or any other host.
func TestRedactErrHidesChainNames(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
		LLMUpstreams: map[string]string{anthropicHost: "https://gw.corp.internal:8443/anthropic"}})
	for _, c := range []struct {
		name    string
		err     error
		leaks   []string
		keeps   string
		differs bool
	}{
		{"unknown authority", unknownAuthorityWithHint(t), []string{chainCAName}, "certificate signed by unknown authority", true},
		{"unknown authority, wrapped", fmt.Errorf("tls: failed to verify certificate: %w", unknownAuthorityWithHint(t)),
			[]string{chainCAName}, "tls: failed to verify certificate", true},
		{"name constraint", nameConstrained(t), []string{"gw.secret.internal", "permitted.corp.internal"}, "not authorized to sign for this name", true},
		{"name constraint, wrapped", fmt.Errorf("dial: %w", nameConstrained(t)), []string{"gw.secret.internal"}, "dial: ", true},
		{"unconstrained name", x509.CertificateInvalidError{Reason: x509.UnconstrainedName, Detail: "gw.secret.internal"},
			[]string{"gw.secret.internal"}, "leaf contains unknown or unconstrained name", true},
		// A reason that prints no name is left as it was.
		{"expired", x509.CertificateInvalidError{Reason: x509.Expired, Detail: "current time is after notAfter"},
			nil, "current time is after notAfter", false},
	} {
		got := p.redactErr(c.err)
		for _, l := range c.leaks {
			if strings.Contains(got, l) {
				t.Errorf("%s: %q still names %q", c.name, got, l)
			}
		}
		if !strings.Contains(got, c.keeps) {
			t.Errorf("%s: %q lost %q", c.name, got, c.keeps)
		}
		if c.differs && !strings.Contains(got, redactedEndpoint) {
			t.Errorf("%s: %q does not show a redaction", c.name, got)
		}
	}
}

// The HTTP/2 resend's failure is a Cause the creator reads: a certificate
// error in it is redacted like any other.
func TestH2MismatchCauseRedactsTheResendError(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
		LLMUpstreams: map[string]string{anthropicHost: "https://gw.corp.internal:8443/anthropic"}})
	mismatch := x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"gw-alt.corp.internal"}}, Host: "gw.corp.internal"}
	for name, h2Err := range map[string]error{
		"unknown authority": unknownAuthorityWithHint(t),
		"name constraint":   nameConstrained(t),
		"hostname":          mismatch,
	} {
		e := &h2MismatchError{hadTLS: true, alpn: "h2", err: errors.New("EOF"), h2Err: h2Err}
		got := p.upstreamProtocolMismatchCause(e)
		for _, l := range []string{chainCAName, "gw.secret.internal", "permitted.corp.internal", "gw-alt.corp.internal", "gw.corp.internal"} {
			if strings.Contains(got, l) {
				t.Errorf("%s: cause %q names %q", name, got, l)
			}
		}
		if !strings.Contains(got, "the HTTP/2 resend also failed: ") || !strings.Contains(got, redactedEndpoint) {
			t.Errorf("%s: cause %q lost its sentence or shows no redaction", name, got)
		}
	}
}

// Gateway membership is the address, not its spelling.
func TestGatewayMembershipIgnoresIPSpelling(t *testing.T) {
	for _, c := range []struct {
		gateway string
		hosts   []string
		not     []string
	}{
		{"https://[fd12:3456::7]:8443/a",
			[]string{"fd12:3456::7", "FD12:3456::7", "fd12:3456:0::7", "fd12:3456:0:0:0:0:0:7", "FD12:3456:0000:0000:0000:0000:0000:0007", "[fd12:3456::7]"},
			[]string{"fd12:3456::8", "fd12:3456:7::", "gw.corp.internal"}},
		{"https://[FD12:3456:0::7]/a", []string{"fd12:3456::7", "fd12:3456:0:0:0:0:0:7"}, nil},
		{"https://10.1.2.3/a", []string{"10.1.2.3", "::ffff:10.1.2.3", "::FFFF:a01:203", "0:0:0:0:0:ffff:10.1.2.3"}, []string{"10.1.2.4", "::ffff:10.1.2.4", "::10.1.2.3"}},
		{"https://[::ffff:10.1.2.3]/a", []string{"10.1.2.3", "::ffff:a01:203"}, nil},
		{"https://gw.corp.internal/a", []string{"gw.corp.internal", "GW.corp.internal."}, []string{"gw2.corp.internal"}},
	} {
		p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
			LLMUpstreams: map[string]string{anthropicHost: c.gateway}})
		for _, h := range c.hosts {
			if v, ok := p.gatewayVendorOf(h); !ok || v != anthropicHost {
				t.Errorf("gateway %s: host %q = (%q, %v), want the gateway", c.gateway, h, v, ok)
			}
		}
		for _, h := range c.not {
			if _, ok := p.gatewayVendorOf(h); ok {
				t.Errorf("gateway %s: host %q matched", c.gateway, h)
			}
		}
	}
	// The redaction that membership gates follows the address too.
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
		LLMUpstreams: map[string]string{anthropicHost: "https://[fd12:3456::7]:8443/a"}})
	err := x509.HostnameError{Certificate: &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("fd00::1")}}, Host: "FD12:3456:0:0::7"}
	if got := p.redactErr(err); strings.Contains(got, "fd00") {
		t.Errorf("redactErr = %q, still lists the certificate's addresses", got)
	}
}

// The classification and the route target use the same membership rule as the
// error path: a gateway reached by another spelling of its address is still the
// gateway (and so still gets the gateway vet, not the sandbox-target one).
func TestGatewayIPSpellingIsOneHostForClassifyAndRoute(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
		LLMUpstreams: map[string]string{anthropicHost: "https://[fd12:3456::7]:8443/a", openaiHost: "https://10.1.2.3:8443/a"}})
	for _, h := range []string{"fd12:3456::7", "FD12:3456:0:0:0:0:0:7", "10.1.2.3", "::ffff:10.1.2.3"} {
		if !p.isLLMHost(h) {
			t.Errorf("isLLMHost(%q) = false, want the configured gateway", h)
		}
	}
	if p.isLLMHost("10.1.2.4") || p.isLLMHost("::ffff:10.1.2.4") {
		t.Error("isLLMHost matched an address that is no gateway's")
	}
	for _, h := range []string{"10.1.2.3", "::ffff:10.1.2.3"} {
		if got, err := p.llmRouteTarget(h, 8443); err != nil || got != "10.1.2.3:8443" {
			t.Errorf("llmRouteTarget(%q) = %q, %v; want the gateway vet to accept 10.1.2.3:8443", h, got, err)
		}
	}
}
