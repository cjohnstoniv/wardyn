// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	neturl "net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateLLMInspection enforces the structural invariants of the optional
// outbound content-inspection block. A nil spec (the default) is valid (off).
// Takes the FULL RunPolicySpec (not just LLMInspection) because
// detector_sidecar_url is now validated against this SAME spec's own egress
// allowlist — see the raw-value comment below.
func validateLLMInspection(spec types.RunPolicySpec) error {
	li := spec.LLMInspection
	if li == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(li.Mode))
	switch mode {
	case "", "off", "alert", "block":
	default:
		return fmt.Errorf("llm_inspection.mode: unknown mode %q", li.Mode)
	}
	// A raw VALUE may never be authored on a policy write —
	// stored, inline, or WARDYN_DEFAULT_POLICY. Only dispatch ever populates
	// this field, internally, in memory, on the ephemeral copy handed to the
	// proxy sidecar (resolveLLMInspectionSecrets, runs_dispatch.go). An
	// operator authors workspace_secret_names instead (types.LLMInspectionSpec).
	if len(li.WorkspaceSecretValues) > 0 {
		return fmt.Errorf("llm_inspection.workspace_secret_values may not be set on a policy write " +
			"(it is resolved internally, store->proxy, at dispatch — see workspace_secret_names)")
	}
	// The write-time half of the SINK guard. workspace_secret_names is
	// resolved to PLAINTEXT onto the policy copy handed to the proxy sidecar
	// (resolveLLMInspectionSecrets), which makes it a credential sink like the
	// api_key/git_pat/ssh_key lanes — and sinkReservedSecret is the one guard
	// they all take ("reject it at every sink", secrets.go). Refusing here means
	// an operator authoring a reserved name gets a 400 instead of a run whose
	// scanner silently covers one fewer value than they asked for.
	for i, name := range li.WorkspaceSecretNames {
		// #1048: an impossible name (upper-case, a leading space, a unicode
		// hyphen, a path) used to pass write time and just cover one fewer
		// value at dispatch, silently — the same gap env_secret had.
		if !secretNameRE.MatchString(name) {
			return fmt.Errorf("llm_inspection.workspace_secret_names[%d]: %q is not a valid secret name", i, name)
		}
		if nameSinkReservedSecret(name) {
			return fmt.Errorf("llm_inspection.workspace_secret_names[%d]: %q is a reserved platform-internal secret name", i, name)
		}
		if name == bedrockAPIKeySecret {
			return fmt.Errorf("llm_inspection.workspace_secret_names[%d]: %q is read only for the grant Wardyn authors "+
				"when a run launches on the Bedrock bearer key, never by name", i, name)
		}
	}
	if err := validateDetectorSidecarFormat(li, mode); err != nil {
		return err
	}
	if mode != "" && mode != "off" {
		if !li.DetectSecrets && !li.DetectSecretPatterns && !li.DetectEntropy &&
			!li.DetectPII && li.DetectorSidecarURL == "" && len(li.ClassifiedMarkers) == 0 {
			return fmt.Errorf("llm_inspection: at least one detector must be enabled when mode is %q", mode)
		}
		if u := strings.TrimSpace(li.DetectorSidecarURL); u != "" {
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				return fmt.Errorf("llm_inspection.detector_sidecar_url must be an http(s) URL")
			}
			// Belt-and-braces: the sidecar is dialed PROXY-SIDE with the
			// outbound span TEXT (internal/contentscan/sidecar.go) — a surface the
			// sandbox's own confinement class never bounds, so "trusted operator
			// config" (that file's own framing) only actually holds once this
			// URL's host is ALSO on this same policy's own egress allowlist — the
			// same exact-entry bar a brokered credential injection already
			// requires (domainAllowedExact) — rather than accepting any http(s)
			// URL at face value. Doesn't widen egress by itself (the sidecar dial
			// never goes through the sandbox's own allowlist); it requires the
			// operator to have already named the host for something.
			parsed, perr := neturl.Parse(u)
			if perr != nil || parsed.Hostname() == "" {
				return fmt.Errorf("llm_inspection.detector_sidecar_url is not a valid URL")
			}
			if !spec.AllowAllEgress && !domainAllowedExact(spec.AllowedDomains, parsed.Hostname()) {
				return fmt.Errorf("llm_inspection.detector_sidecar_url host %q must be in this policy's own allowed_domains "+
					"(or allow_all_egress) — an operator allowlist, not a bare http(s) check", parsed.Hostname())
			}
		}
		// require_inspectable_llm is a RUNTIME guarantee, and only TLS-MITM can
		// inspect an opaque CONNECT (incl. an api-key run that overrides its base
		// URL to CONNECT directly). So requiring inspectability requires MITM.
		if li.RequireInspectableLLM && !li.InterceptTLS {
			return fmt.Errorf("llm_inspection: require_inspectable_llm requires intercept_tls (only TLS-MITM gives a runtime inspection guarantee)")
		}
	}
	if li.MaxScanBytes < 0 {
		return fmt.Errorf("llm_inspection.max_scan_bytes must be >= 0")
	}
	switch strings.ToLower(strings.TrimSpace(li.OnScannerError)) {
	case "", "pass", "block":
	default:
		return fmt.Errorf("llm_inspection.on_scanner_error: must be \"pass\" or \"block\"")
	}
	switch strings.ToLower(strings.TrimSpace(li.BlockMinSeverity)) {
	case "", "low", "medium", "high", "critical":
	default:
		return fmt.Errorf("llm_inspection.block_min_severity: unknown severity %q", li.BlockMinSeverity)
	}
	return nil
}

// validateDetectorSidecarFormat checks detector_sidecar_format. Unset is Wardyn's own format and
// asks nothing. "presidio" needs the URL it posts to, and in block mode needs a threshold of at
// least medium: Presidio reports dates, names, URLs, ids and addresses in ordinary code text, and
// the default (low) threshold would refuse those prompts. Alert mode has no such limit, and a
// policy that does not opt in is not affected.
func validateDetectorSidecarFormat(li *types.LLMInspectionSpec, mode string) error {
	switch strings.ToLower(strings.TrimSpace(li.DetectorSidecarFormat)) {
	case "":
		return nil
	case types.DetectorSidecarFormatPresidio:
	default:
		return fmt.Errorf("llm_inspection.detector_sidecar_format: must be unset or %q", types.DetectorSidecarFormatPresidio)
	}
	if strings.TrimSpace(li.DetectorSidecarURL) == "" {
		return fmt.Errorf("llm_inspection.detector_sidecar_format %q needs detector_sidecar_url", types.DetectorSidecarFormatPresidio)
	}
	if mode == "block" {
		switch strings.ToLower(strings.TrimSpace(li.BlockMinSeverity)) {
		case "medium", "high", "critical":
		default:
			return fmt.Errorf("llm_inspection: detector_sidecar_format %q in block mode needs block_min_severity of "+
				"medium or higher (its low-severity entities, such as names, dates and URLs, would otherwise block ordinary prompts)",
				types.DetectorSidecarFormatPresidio)
		}
	}
	return nil
}
