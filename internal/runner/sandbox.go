// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// Sandbox-launch primitives shared by every substrate (SECURITY-RELEVANT where
// noted): pure data/string transforms a future non-Docker substrate needs
// byte-identical, hoisted verbatim out of internal/runner/docker into this
// TAGLESS file so no substrate re-derives (and risks drifting) the agent
// contract.

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

// AgentIdleScript is the agent sandbox's main (idle) process for
// non-interactive runs: installs the per-run TLS-MITM CA (when delivered),
// required so claude trusts the proxy's TLS termination of api.anthropic.com,
// then idles while the task Exec does the real work. Writes the exact paths
// internal/api pins (/tmp/wardyn, any-uid-writable) and assembles the
// COMBINED bundle (system roots + per-run CA) that
// SSL_CERT_FILE/REQUESTS_CA_BUNDLE/CURL_CA_BUNDLE point at, since those vars
// REPLACE the client trust store. Keep in lockstep with install_mitm_ca in
// deploy/images/common/agent-run-lib.sh. No-op when WARDYN_MITM_CA_PEM is
// unset. The idle loop (not `exec sleep infinity`) is TERM-aware: as PID 1,
// `sleep` ignores SIGTERM, so a bare sleep would wait out the full kill
// timeout instead of exiting promptly; exits 143/130, not 0, so an
// out-of-band stop still reads as a signal kill downstream.
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
trap 'exit 143' TERM
trap 'exit 130' INT
while :; do sleep 3600 & wait $!; done`

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

// RecorderArgv builds the argv an agent sandbox runs when session recording
// is enabled. It delegates to wardyn-rec (a thin binary in the agent image)
// so the GPL recorder (asciinema) is exec'd as a subprocess, never linked
// into Wardyn.
//
// Layout:
//
//	wardyn-rec -cast-dir <dir> [-out-dir <mount>] [-upload-url <proxy route>] -run <id> -- <agent argv...>
//
// uploadURL is the DEFAULT delivery path (the proxy's brokered recording
// route), which lets the control plane MASK secrets before persisting the
// cast. outDir is the shared-mount fallback: no cross-run isolation, and its
// cast is UNMASKED since wardyn-rec holds no secret values. The two are
// MUTUALLY EXCLUSIVE — when uploadURL is set, -out-dir is dropped entirely so
// an unmasked cast can never reach the API-served replay store.
//
// Callers should only invoke this when recording is enabled.
func RecorderArgv(castDir, outDir, uploadURL string, runID uuid.UUID, agentArgv []string) []string {
	out := []string{
		"wardyn-rec",
		"-cast-dir", castDir,
	}
	// Prefer the masked upload over the unmasked shared mount when both are
	// offered; -out-dir is only emitted as the fallback.
	if uploadURL != "" {
		out = append(out, "-upload-url", uploadURL)
	} else if outDir != "" {
		out = append(out, "-out-dir", outDir)
	}
	out = append(out, "-run", runID.String(), "--")
	return append(out, agentArgv...)
}

// proxySidecarEnvKnobNames is the single source of truth for
// ProxySidecarEnvKnobs' key list, so a consumer needing the NAMES without a
// live env (cmd/wardynd/envdoc_guard_test.go) reads the same list rather than
// a hand-copied one that can drift.
var proxySidecarEnvKnobNames = []string{
	"WARDYN_LLM_SCAN",
	"WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS",
	"WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS",
	// The mid-run credential re-auth hold's budget. A knob missing from this
	// list is not "default", it is UNREACHABLE on a managed substrate.
	"WARDYN_CREDENTIAL_REAUTH_TIMEOUT",
}

// ProxySidecarEnvKnobNames returns a COPY of the knob-name list: exported for the
// envdoc guard, cloned so no caller can append to or reorder the list that decides
// what reaches every proxy sidecar.
func ProxySidecarEnvKnobNames() []string { return slices.Clone(proxySidecarEnvKnobNames) }

// ProxySidecarEnvKnobs returns the operator knobs the wardyn-proxy sidecar
// reads from ITS OWN environment — as name/value pairs, only for the ones
// wardynd has set — for a substrate to copy into the sidecar it creates.
//
// The sidecar does not inherit wardynd's environment on any substrate, so a
// knob a substrate forgets is UNREACHABLE rather than merely "off" — hence one
// list here, beside BuildProxyConfig, for every substrate. Values pass through
// verbatim; each knob's own reader does the parsing and fail-closed decision.
func ProxySidecarEnvKnobs() [][2]string {
	var out [][2]string
	for _, k := range proxySidecarEnvKnobNames {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}

// BuildProxyConfig marshals a run's ProxyConfig (egress policy, MITM CA,
// injection rules, run token, ...) into the JSON payload every substrate
// delivers to its wardyn-proxy sidecar (on stdin on Docker, as a staged file on
// Kubernetes; never in its environment) — the proxy fails closed without it.
// port is the sidecar's listen port (ProxyListenPort in production;
// parameterized for tests).
func BuildProxyConfig(runID uuid.UUID, pc ProxyConfig, port int) ([]byte, error) {
	inj := make([]proxy.InjectionConfig, 0, len(pc.Injection))
	for _, g := range pc.Injection {
		inj = append(inj, proxy.InjectionConfig{InjectionRule: g.Rule, GrantID: g.GrantID})
	}
	cfg := proxy.Config{
		RunID:                runID,
		ControlPlaneURL:      pc.ControlPlaneURL,
		ControlPlaneCAPEM:    pc.ControlPlaneCAPEM,
		RunToken:             pc.RunToken,
		Policy:               pc.Policy,
		Injection:            inj,
		Listen:               ":" + strconv.Itoa(port),
		MITMCACertPEM:        pc.MITMCACertPEM,
		MITMCAKeyPEM:         pc.MITMCAKeyPEM,
		MITMHosts:            pc.MITMHosts,
		MITMLLM:              pc.MITMLLM,
		GitGrants:            pc.GitGrants,
		PATGrants:            pc.PATGrants,
		ADOGrant:             pc.ADOGrant,
		UpstreamProxyURL:     pc.UpstreamProxyURL,
		TrustedCAPEM:         pc.TrustedCAPEM,
		InternalHosts:        pc.InternalHosts,
		UpstreamProxyNoProxy: pc.UpstreamProxyNoProxy,
		LLMUpstreams:         pc.LLMUpstreams,
		LLMUnavailableDetail: pc.LLMUnavailableDetail,
		Unattended:           pc.Unattended,
	}
	return json.Marshal(cfg)
}
