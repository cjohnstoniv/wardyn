// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// Sandbox-launch primitives shared by every substrate (SECURITY-RELEVANT where
// noted).
//
// This file holds the parts of the docker substrate that carry NO docker-
// specific behavior — pure data/string transforms a future non-Docker
// substrate (e.g. Kubernetes, build tag `k8s`) needs byte-identical, so a
// second substrate never re-derives (and risks drifting) the agent contract.
// Hoisted verbatim from internal/runner/docker; the docker package now calls
// these instead of carrying its own copies. Follows mount.go's precedent: a
// TAGLESS file in this package, importable by any build-tag-gated substrate
// without pulling that substrate's dependencies along.

// ProxyListenPort is the port wardyn-proxy listens on inside the per-run
// internal network/namespace. The agent's ONLY reachable address.
const ProxyListenPort = 3128

// Conservative platform resource defaults, applied when a Resources field is
// zero. They exist so EVERY agent sandbox is capped even when policy sets
// nothing: without them one runaway or prompt-injected agent can OOM-kill the
// host, fork-bomb the host PID space, or fill host storage and take sibling
// runs down with it (the basic multi-tenant safety controls — see
// types.ResourceLimits). A policy value always overrides.
const (
	DefaultCPUMillis int64 = 2000 // 2 vCPU
	DefaultMemoryMiB int64 = 4096 // 4 GiB hard memory cap
	DefaultPidsLimit int64 = 512  // max processes/threads (fork-bomb guard)
)

// AgentIdleScript is the agent sandbox's main (idle) process for NON-
// interactive runs: it installs the per-run TLS-MITM CA (when delivered) and
// then idles while the task Exec does the real work. (Interactive runs use
// `agent-run --idle` as their main process instead — each substrate's
// CreateSandbox wires this — which performs this same CA install plus
// workspace prep; the human then drives claude in the attach shell.) The CA
// install is REQUIRED either way: without it, NODE_EXTRA_CA_CERTS points at a
// CA file that was never written, so claude cannot trust the proxy's TLS
// termination of api.anthropic.com (breaking subscription proxy-side
// injection). It writes the EXACT paths internal/api pins (/tmp/wardyn — any-
// uid-writable, so it works regardless of the image's USER/HOME; this Cmd may
// run as root while agent-run later re-runs as the image user, hence the
// sticky-bit dir and the ||true rewrites: within one run the content is
// identical, so a failed rewrite over a correct file is harmless). It also
// assembles the COMBINED bundle (system roots + per-run CA) that
// SSL_CERT_FILE/REQUESTS_CA_BUNDLE/CURL_CA_BUNDLE point at — those vars
// REPLACE the client trust store, so the bare CA there would break non-MITM'd
// CONNECT-tunneled hosts. Keep in lockstep with install_mitm_ca in
// deploy/images/common/agent-run-lib.sh. No-op when the run did not opt into
// TLS-MITM (WARDYN_MITM_CA_PEM unset).
const AgentIdleScript = `d=/tmp/wardyn
if [ -n "${WARDYN_MITM_CA_PEM:-}" ]; then
  mkdir -p "$d" 2>/dev/null; chmod 1777 "$d" 2>/dev/null || true
  { printf '%s\n' "$WARDYN_MITM_CA_PEM" > "$d/mitm-ca.pem" && chmod 0644 "$d/mitm-ca.pem"; } 2>/dev/null || true
  sys=""
  for c in /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem /etc/pki/tls/certs/ca-bundle.crt; do
    [ -f "$c" ] && sys="$c" && break
  done
  if [ -n "$sys" ]; then
    { cat "$sys" "$d/mitm-ca.pem" > "$d/ca-bundle.pem" && chmod 0644 "$d/ca-bundle.pem"; } 2>/dev/null || true
  else
    { cp "$d/mitm-ca.pem" "$d/ca-bundle.pem" && chmod 0644 "$d/ca-bundle.pem"; } 2>/dev/null || true
    echo "wardyn: no system CA bundle found; ca-bundle.pem is proxy-CA-only (non-MITM TLS hosts will not verify)" >&2
  fi
  if command -v update-ca-certificates >/dev/null 2>&1; then
    cp "$d/mitm-ca.pem" /usr/local/share/ca-certificates/wardyn-mitm.crt 2>/dev/null && update-ca-certificates >/dev/null 2>&1 || true
  fi
fi
exec sleep infinity`

// knownNonVaultRuntimes are OCI runtime families known to NOT boot a
// per-sandbox KVM VM: runc/crun/sysbox share the host kernel, and runsc
// (gVisor) is a userspace-kernel sandbox (the Wall/CC2 tier). Named as literal
// strings (rather than the docker package's runtimeRunsc/runtimeSysbox
// constants) so this tagless package never imports a build-tag-gated one. A
// substrate's CC3 pin naming one of these is a silent downgrade and is
// refused even under an explicit pin — unlike an unrecognized runtime name,
// which the operator is trusted to vouch for.
var knownNonVaultRuntimes = []string{"runc", "crun", "sysbox", "runsc"}

// IsKnownNonVaultRuntime reports whether name is a runtime positively known to
// deliver less than a VM boundary (the CC3/Vault floor guard). Prefix match so
// "runc"/"crun-foo"/"sysbox-runc"/"runsc-*" are all caught. Note "krun" is NOT
// matched by the "crun" family (different leading byte), so libkrun (crun +
// libkrun, a real KVM microVM) stays eligible for CC3.
func IsKnownNonVaultRuntime(name string) bool {
	for _, fam := range knownNonVaultRuntimes {
		if strings.HasPrefix(name, fam) {
			return true
		}
	}
	return false
}

// RecorderArgv builds the argv an agent sandbox runs when session recording is
// enabled. It delegates to wardyn-rec (a thin binary inside the agent image)
// so the GPL recorder (asciinema) is exec'd as a subprocess, never linked into
// Wardyn (license hygiene).
//
// Layout:
//
//	wardyn-rec -cast-dir <dir> [-out-dir <mount>] [-upload-url <proxy route>] -run <id> -- <agent argv...>
//
// wardyn-rec decides at runtime whether asciinema is present; the caller does
// not need to know. uploadURL is the DEFAULT delivery path: the proxy's
// brokered recording route (PUT /wardyn/v1/recordings/{run}), which injects
// the run token and lets the control plane MASK secrets before persisting the
// cast (secret masking lives control-plane-side; the registry of secret
// values is never in the sandbox). outDir is the optional shared-mount
// fallback and carries TWO documented limitations: (1) every agent sandbox
// sharing the mount runs under the same identity, so it has NO cross-run
// isolation; and (2) it bypasses the control plane, so the cast it writes is
// UNMASKED — secret masking is structurally impossible here (wardyn-rec holds
// no secret values, by design). Use the brokered upload path where recordings
// are viewer-exposed.
//
// HIGH-finding hardening: the masked upload path and the unmasked shared-mount
// -out-dir are MUTUALLY EXCLUSIVE. When an uploadURL is configured we drop the
// shared-mount -out-dir entirely so wardyn-rec can NEVER also drop an UNMASKED
// <runID>.cast into the API-served replay store (cross-run-writable, viewer-
// exposed). The shared mount is only ever used as the reduced-isolation
// FALLBACK when no control-plane upload path exists.
//
// Callers should only invoke this when recording is enabled — there is no
// passthrough case; an unwrapped argv is simply argv itself.
func RecorderArgv(castDir, outDir, uploadURL string, runID uuid.UUID, agentArgv []string) []string {
	out := []string{
		"wardyn-rec",
		"-cast-dir", castDir,
	}
	// Prefer the masked control-plane upload over the unmasked shared mount: if
	// both are offered, suppress -out-dir so no unmasked cast reaches a path the
	// API serves. -out-dir is only emitted as the fallback (uploadURL == "").
	if uploadURL != "" {
		out = append(out, "-upload-url", uploadURL)
	} else if outDir != "" {
		out = append(out, "-out-dir", outDir)
	}
	out = append(out, "-run", runID.String(), "--")
	return append(out, agentArgv...)
}

// BuildProxyConfig marshals a run's ProxyConfig (egress policy, MITM CA,
// injection rules, run token, ...) into the JSON payload every substrate
// delivers to its wardyn-proxy sidecar as WARDYN_PROXY_CONFIG_JSON — the proxy
// fails closed without it (no policy => no working egress). Pure data
// transform (field mapping + json.Marshal); hoisted out of the docker driver
// so a k8s substrate builds byte-identical sidecar config without duplicating
// the mapping. port is the sidecar's listen port (ProxyListenPort in
// production; parameterized for tests).
func BuildProxyConfig(runID uuid.UUID, pc ProxyConfig, port int) ([]byte, error) {
	inj := make([]proxy.InjectionConfig, 0, len(pc.Injection))
	for _, g := range pc.Injection {
		inj = append(inj, proxy.InjectionConfig{InjectionRule: g.Rule, GrantID: g.GrantID})
	}
	cfg := proxy.Config{
		RunID:            runID,
		ControlPlaneURL:  pc.ControlPlaneURL,
		RunToken:         pc.RunToken,
		Policy:           pc.Policy,
		Injection:        inj,
		Listen:           ":" + strconv.Itoa(port),
		MITMCACertPEM:    pc.MITMCACertPEM,
		MITMCAKeyPEM:     pc.MITMCAKeyPEM,
		MITMHosts:        pc.MITMHosts,
		MITMLLM:          pc.MITMLLM,
		GitGrants:        pc.GitGrants,
		PATGrants:        pc.PATGrants,
		UpstreamProxyURL: pc.UpstreamProxyURL,
	}
	return json.Marshal(cfg)
}
