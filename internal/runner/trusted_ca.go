// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
)

// TrustedCAMountPath is where a laptop runner's own CA bundle is bind-mounted, read-only, into a
// sandbox. AgentIdleScript and install_mitm_ca (deploy/images/common/agent-run-lib.sh) append it to
// the sandbox's CA trust; keep the three in lockstep. It sits directly in /etc, outside /run (the
// component-secret anchor) and beside, not inside, /etc/claude-code: a mount on or above a
// managed-file directory, or a directory Docker creates for one, would refuse every run that
// delivers a managed file (see docker's deliverManagedFiles and volumeOverSecretDir).
const TrustedCAMountPath = "/etc/wardyn-trusted-ca.pem"

// MaxEnvTrustedCAPEM is the largest runner bundle wardyn-runnerd accepts, and the largest
// WARDYN_MITM_CA_PEM it builds by appending one. That variable is one environment string, and Linux
// refuses to start a process with one over 128 KiB. It is also the only way the bundle reaches a
// sandbox's clients on a run with no PEM staged, so runnerd refuses a bigger bundle at startup.
// When the bundle plus an organisation-staged PEM exceed it, the bundle rides only the mount at
// TrustedCAMountPath, and an agent image older than 0.9.3 then trusts only the organisation's roots.
const MaxEnvTrustedCAPEM = 64 << 10

// ParseCertsPEM decodes every "CERTIFICATE" PEM block in raw. Any block that
// fails to parse as a certificate, any block that is NOT a certificate (a
// private key or CSR pasted alongside the cert — the common combined-export
// shape), or a raw input with no certificate block at all is an error —
// never a silent skip: a corp CA file that doesn't parse as configured is a
// configuration mistake, not a partial success, and a private key must never
// ride into every sandbox as trusted-issuer material. Shared by wardynd's
// WARDYN_TRUSTED_CA_FILE and wardyn-runnerd's --trusted-ca-file.
func ParseCertsPEM(raw []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected %q PEM block: the file must hold certificates only", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no PEM-encoded certificates found")
	}
	return certs, nil
}

// TrustedCABundle validates raw with ParseCertsPEM and returns the bundle
// REBUILT from the parsed certificates — never the raw file — so nothing but
// CERTIFICATE blocks can reach a sandbox env (WARDYN_MITM_CA_PEM) or a proxy
// config (trusted_ca_pem).
func TrustedCABundle(raw []byte) (string, []*x509.Certificate, error) {
	certs, err := ParseCertsPEM(raw)
	if err != nil {
		return "", nil, err
	}
	var bundle []byte
	for _, c := range certs {
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return string(bundle), certs, nil
}

// sandboxCATrustVars is THE list of CA-trust variables a sandbox's toolchains
// read, and the single place any of them is named. It exists because this list
// was duplicated across two callers and drifted: AWS_CA_BUNDLE was
// added to one and missed on the other, which left the sandbox's AWS CLI
// distrusting Wardyn's OWN per-run interception CA on the Bedrock lane the
// product drives itself. One list, two callers, nothing to keep in sync.
//
// "Install the CA" is one action PER TOOLCHAIN, and the values are not
// interchangeable. NODE_EXTRA_CA_CERTS is ADDITIVE (Node keeps its bundled
// roots) so it takes the BARE CA. Every other variable REPLACES its trust
// store outright, so each takes the COMBINED bundle (system roots + the
// per-run CA) that install_mitm_ca/agentIdleScript assemble — the bare CA
// there would break verification of every non-MITM'd CONNECT-tunneled host.
// AWS CLI v2 needs its own entry because it ships its own Python and its own
// CA store and reads none of the other four. The JVM keystore and Deno's
// DENO_CERT are covered by no variable at all and are documented as not
// covered (deploy/images/README.md).
func sandboxCATrustVars() map[string]string {
	return map[string]string{
		"NODE_EXTRA_CA_CERTS": "/tmp/wardyn/mitm-ca.pem",
		"SSL_CERT_FILE":       "/tmp/wardyn/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE":  "/tmp/wardyn/ca-bundle.pem",
		"CURL_CA_BUNDLE":      "/tmp/wardyn/ca-bundle.pem",
		"AWS_CA_BUNDLE":       "/tmp/wardyn/ca-bundle.pem",
	}
}

// SetSandboxCATrustVars writes that list into sandboxEnv. onlyIfUnset leaves
// any value already staged alone, which is what the corporate-CA append needs:
// a MITM'd run's own /tmp/wardyn paths must never be clobbered.
func SetSandboxCATrustVars(sandboxEnv map[string]string, onlyIfUnset bool) {
	for k, v := range sandboxCATrustVars() {
		if onlyIfUnset && sandboxEnv[k] != "" {
			continue
		}
		sandboxEnv[k] = v
	}
}

// InstallSandboxTrustedCA appends the operator's corporate PEM
// (wardynd's WARDYN_TRUSTED_CA_FILE, or a laptop runner's own bundle,
// corpPEM) to this run's sandbox CA trust, so a
// TLS-inspecting corporate middlebox on the path between wardyn-proxy and the
// real internet is trusted by the sandbox's OWN TLS clients on a passthrough
// (non-MITM'd) CONNECT tunnel — not only by wardynd and wardyn-proxy
// themselves (see cmd/wardynd/trusted_ca.go / proxy/server.go). corpPEM == "" (the knob
// unset) is a no-op: sandboxEnv is left exactly as provisionDispatchMITMCA
// (or nothing, on a non-MITM run) left it.
//
// install_mitm_ca (agent-run-lib.sh) treats WARDYN_MITM_CA_PEM as an opaque
// PEM bundle already — multiple certificates just concatenate — so appending
// here is additive to whatever provisionDispatchMITMCA staged for this run's
// OWN per-run interception CA. On a run with NO Wardyn-side MITM (that block
// above never ran), WARDYN_MITM_CA_PEM would otherwise be entirely absent and
// install_mitm_ca would no-op, so this also SEEDS it plus the five bundle
// vars the sandbox's toolchains need — but only when provisionDispatchMITMCA
// has not already set them, so a MITM'd run's own /tmp/wardyn paths are never
// touched here.
//
// Five, not four: "install the CA" is one action PER TOOLCHAIN, and AWS CLI v2
// ships its OWN Python and its OWN CA store — it reads none of the other four.
// Without AWS_CA_BUNDLE a MITM'd Bedrock/STS call still fails, in a lane the
// product drives itself. deploy/images/README.md carries the per-toolchain
// table; the JVM keystore is addressed by no variable at all.
func InstallSandboxTrustedCA(corpPEM string, sandboxEnv map[string]string) {
	if corpPEM == "" {
		return
	}
	if existing := sandboxEnv["WARDYN_MITM_CA_PEM"]; existing != "" {
		sandboxEnv["WARDYN_MITM_CA_PEM"] = existing + "\n" + corpPEM
	} else {
		sandboxEnv["WARDYN_MITM_CA_PEM"] = corpPEM
	}
	SetSandboxCATrustVars(sandboxEnv, true)
}

// WithTrustedCA returns the proxy config b with trusted_ca_pem set to a runner's own bundle. Both
// runner substrates apply it on create (and Docker on ReplaceProxy too), since a revive rebuilds from
// the organisation's stored config, which never carries a CA for a runner. The value REPLACES
// whatever is there: the organisation's roots stay off a runner's proxy. An empty bundle leaves b
// untouched.
func WithTrustedCA(b []byte, pemBundle string) ([]byte, error) {
	if pemBundle == "" {
		return b, nil
	}
	return WithProxyConfigKeys(b, map[string]any{"trusted_ca_pem": pemBundle})
}

// WithProxyConfigKeys returns the proxy config b with each key of kv set to its value, replacing
// any value already there. A runner adds the keys only it knows (its CA bundle, the k8s runner's
// self-lapse, decision sink and hard denies) to the config BuildProxyConfig made. The proxy decodes
// strictly, so a key it does not know refuses its start rather than being ignored.
func WithProxyConfigKeys(b []byte, kv map[string]any) ([]byte, error) {
	if len(kv) == 0 {
		return b, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("proxy config is empty")
	}
	for k, v := range kv {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		data[k] = raw
	}
	return json.Marshal(data)
}
