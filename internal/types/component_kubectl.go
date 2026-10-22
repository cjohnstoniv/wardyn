// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

// The kubectl component kind: its switch, its token naming and its validation.

// EnvKubectlCLI turns the kubectl kind on, off by default. Off, a kubectl row
// is refused wherever a definition is checked exactly as before the kind
// existed (an unknown cli.kind). Read on every check, and once at boot.
const EnvKubectlCLI = "WARDYN_KUBECTL_CLI"

// KubectlCLIOn reports whether EnvKubectlCLI is on.
func KubectlCLIOn() bool { return cliutil.EnvBool(EnvKubectlCLI, false) }

var (
	// A Kubernetes namespace is a DNS-1123 label; a service account name a
	// DNS-1123 subdomain.
	cliKubeLabelRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	cliKubeSubdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// CLIKubectlTokenPart ends the reserved name a kubectl row's registered
// service-account token is stored under: wardyn-cli-<component id>-kubectl.
const CLIKubectlTokenPart = "kubectl"

// CLIKubectlTokenSecret reports whether name is a kubectl row's registered token.
func CLIKubectlTokenSecret(name string) bool {
	id, ok := strings.CutPrefix(name, CLISecretPrefix)
	return ok && strings.HasSuffix(id, "-"+CLIKubectlTokenPart) && len(id) > len(CLIKubectlTokenPart)+1
}

// CLIKubectlNamespace reports whether ns is a Kubernetes namespace name.
func CLIKubectlNamespace(ns string) bool { return cliKubeLabelRE.MatchString(ns) }

// CLIKubectlHost reports whether h is an API server host as HostPort returns
// it: an IP address or a lowercase DNS name.
func CLIKubectlHost(h string) bool { return net.ParseIP(h) != nil || cliKubeSubdomainRE.MatchString(h) }

func (k *CLIKubectl) validate() error {
	u, err := url.Parse(k.Server)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || len(k.Server) > MaxComponentBuiltinFieldBytes ||
		strings.ContainsAny(k.Server, " \t\r\n?#") {
		return errors.New("cli.kubectl.server: must be the cluster's API server, https://<host>[:<port>], with no path")
	}
	if _, _, ok := k.HostPort(); !ok {
		return fmt.Errorf("cli.kubectl.server: %q does not name a port between 1 and 65535", u.Port())
	}
	if h := u.Hostname(); !CLIKubectlHost(strings.ToLower(h)) {
		return fmt.Errorf("cli.kubectl.server: %q is not a host name or an IP address", h)
	}
	if k.CAPEM != "" {
		if len(k.CAPEM) > MaxCLIKubectlCABytes {
			return fmt.Errorf("cli.kubectl.ca_pem: exceeds %d bytes", MaxCLIKubectlCABytes)
		}
		if !cliKubeCAParses(k.CAPEM) {
			return errors.New("cli.kubectl.ca_pem: must be one or more PEM CERTIFICATE blocks and nothing else")
		}
	}
	sa := k.ServiceAccount
	if !cliKubeLabelRE.MatchString(sa.Namespace) {
		return fmt.Errorf("cli.kubectl.service_account.namespace: %q is not a namespace name", sa.Namespace)
	}
	if len(sa.Name) > 253 || !cliKubeSubdomainRE.MatchString(sa.Name) {
		return fmt.Errorf("cli.kubectl.service_account.name: %q is not a service account name", sa.Name)
	}
	if n := len(k.Namespaces); n == 0 || n > MaxCLIKubectlNamespaces {
		return fmt.Errorf("cli.kubectl.namespaces: must list 1 to %d namespaces", MaxCLIKubectlNamespaces)
	}
	for i, ns := range k.Namespaces {
		if !CLIKubectlNamespace(ns) {
			return fmt.Errorf("cli.kubectl.namespaces[%d]: %q is not a namespace name", i, ns)
		}
		if slices.Contains(k.Namespaces[:i], ns) {
			return fmt.Errorf("cli.kubectl.namespaces[%d]: %q is listed twice", i, ns)
		}
	}
	if k.Access != ComponentAccessRead && k.Access != ComponentAccessWrite {
		return fmt.Errorf("cli.kubectl.access: %q is not read or write", k.Access)
	}
	return nil
}

// cliKubeCAParses reports whether pemText is only CERTIFICATE blocks, at least
// one, each a certificate.
func cliKubeCAParses(pemText string) bool {
	rest, n := []byte(pemText), 0
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return n > 0 && strings.TrimSpace(string(rest)) == ""
		}
		if b.Type != "CERTIFICATE" {
			return false
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return false
		}
		n++
	}
}
