// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"regexp"
	"strings"
)

// pemBlockRE / gcpKeyRE span MULTI-LINE secret material for MASKING (the
// line-based leakRules only match the header, which for a verify log would
// leave the key body). MaskSecretShaped applies these to the whole log string.
var (
	pemBlockRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
	gcpKeyRE   = regexp.MustCompile(`(?s)"private_key"\s*:\s*"[^"]*"`)
)

// verify.go — the VerifyResult shape a verify run (cmd/wardyn-verify) produces
// by executing the operator-approved SetupCommands inside a confinement
// sandbox, and the control-plane re-validation of that (untrusted) upload.
//
// SECRET SAFETY: verify logs could in principle contain a secret VALUE if a
// build command echoed one — but Wardyn's broker means api-key/git secrets are
// NEVER resident in the sandbox env (they are proxy-injected / brokered at
// request time), so an `env` dump can't reveal them. As defense-in-depth the
// control plane still MASKS secret-shaped tokens (the leaked-value catalog) out
// of the stored/streamed logs, and the audit for a verify run carries COUNTS +
// exit codes only, never log text.

// VerifyStepResult is one setup command's outcome. LogHead/LogTail are a
// bounded rolling head+tail of combined stdout/stderr (so both the setup
// context and the failure survive truncation).
type VerifyStepResult struct {
	Stage   string `json:"stage"`
	Command string `json:"command"`
	// Running marks the step currently executing in a PROGRESS upload (no exit
	// code yet). A final upload never carries Running steps.
	Running    bool   `json:"running,omitempty"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	LogHead    string `json:"log_head,omitempty"`
	LogTail    string `json:"log_tail,omitempty"`
}

// VerifyResult is a verify run's outcome, uploaded by wardyn-verify (PROGRESS
// uploads with Done=false as each step starts/finishes, then a final Done=true
// upload) and re-validated by DeriveVerifyResult control-plane-side.
type VerifyResult struct {
	Steps []VerifyStepResult `json:"steps,omitempty"`
	OK    bool               `json:"ok"`
	// Ran is false when there were no approved commands to run.
	Ran bool `json:"ran"`
	// Done distinguishes a final upload (finalize the workspace status) from an
	// intermediate PROGRESS upload (keep `verifying`, just show the steps so far).
	Done bool `json:"done,omitempty"`
	// Total is how many commands will run (so the UI can show "step N of Total").
	Total int `json:"total,omitempty"`
	// FailureHint explains the FIRST failing/timed-out step in operator terms,
	// when its exit code + log tail match a known environmental signature
	// (missing toolchain, Maven proxy, noexec /tmp) — see classifyFailureHint.
	// Empty when there is no failure or none of the signatures matched (the
	// existing "Suggest a fix from denied egress" flow stays the fallback).
	FailureHint string `json:"failure_hint,omitempty"`
}

const (
	maxVerifySteps  = 32
	maxVerifyLogLen = 8 << 10 // per head/tail field
)

// DeriveVerifyResult re-validates an untrusted VerifyResult upload: caps step
// count, coerces stage/command/exit to bounded/known values, masks
// secret-shaped tokens out of logs, bounds log length, and recomputes OK from
// the step exit codes (never trusting the uploader's OK flag). Mirrors
// DeriveProfile's facts-out-not-authority-out discipline.
func DeriveVerifyResult(raw VerifyResult) VerifyResult {
	out := VerifyResult{Ran: len(raw.Steps) > 0, Done: raw.Done}
	if raw.Total > 0 && raw.Total <= maxVerifySteps {
		out.Total = raw.Total
	}
	ok := out.Ran
	hintDecided := false
	for i, s := range raw.Steps {
		if i >= maxVerifySteps {
			break
		}
		stage := s.Stage
		if _, known := setupStages[stage]; !known {
			stage = "run"
		}
		exit := s.ExitCode
		if exit < 0 {
			exit = -1
		} else if exit > 255 {
			exit = 255
		}
		dur := s.DurationMs
		if dur < 0 {
			dur = 0
		}
		step := VerifyStepResult{
			Stage:      stage,
			Command:    clampLine(s.Command, maxSetupCommandLen),
			Running:    s.Running,
			ExitCode:   exit,
			DurationMs: dur,
			TimedOut:   s.TimedOut,
			LogHead:    MaskSecretShaped(clampLog(s.LogHead)),
			LogTail:    MaskSecretShaped(clampLog(s.LogTail)),
		}
		out.Steps = append(out.Steps, step)
		// A still-running step is neither pass nor fail; only a completed
		// non-zero/timed-out step makes the run not-OK.
		if !step.Running && (exit != 0 || step.TimedOut) {
			ok = false
			// The FIRST failing step decides the hint (mirrors the first-failing-
			// stage convention the caller's audit event already uses) — a later
			// step's failure is usually just fallout from the first.
			if !hintDecided {
				out.FailureHint = classifyFailureHint(step.Command, step.ExitCode, step.LogTail)
				hintDecided = true
			}
		}
	}
	out.OK = ok
	return out
}

// classifyFailureHint maps a failed step's command + exit code + (already
// masked) log tail to a plain-English environmental cause, when it matches a
// known signature — the honest-failure-surfacing counterpart to the UI's
// egress-only "Suggest a fix from denied egress" flow, which has nothing
// useful to say about a missing toolchain, a Maven proxy miss, or a noexec
// /tmp. Pure + signature-based: an unmatched failure returns "" (no hint, not
// a wrong guess) rather than defaulting to the egress explanation.
func classifyFailureHint(command string, exitCode int, logTail string) string {
	cmd := strings.ToLower(command)
	tail := strings.ToLower(logTail)
	switch {
	case exitCode == 127 || strings.Contains(tail, "command not found") || strings.Contains(tail, "not found"):
		return "a required toolchain isn't in the sandbox image (exit 127 / \"not found\") — the default agent " +
			"image carries Node only. Wire a multi-toolchain image via WARDYN_AGENT_IMAGES."
	case (strings.Contains(cmd, "mvn") || strings.Contains(tail, "maven")) &&
		(strings.Contains(tail, "unknown host") || strings.Contains(tail, "could not transfer")):
		return "Maven proxy: the platform now sets MAVEN_OPTS for the sandbox proxy; if this persists the image " +
			"may override it (check for a baked settings.xml or MAVEN_OPTS)."
	case strings.Contains(tail, "permission denied") &&
		(strings.Contains(cmd, "go test") || strings.Contains(tail, "go test") ||
			strings.Contains(tail, "test binary") || strings.Contains(tail, "/tmp")):
		return "the sandbox /tmp is noexec; GOTMPDIR is set by the platform — ensure the image doesn't override " +
			"TMPDIR/GOTMPDIR."
	default:
		return ""
	}
}

// MaskSecretShaped replaces any high-precision leaked-value match (AWS/GitHub/
// Stripe/JWT/…) with a fixed placeholder, so a build log that happened to print
// a secret-shaped token never persists it. Reuses the leaked-value catalog.
func MaskSecretShaped(s string) string {
	// Multi-line material first (span whole key blocks, not just the header).
	s = pemBlockRE.ReplaceAllString(s, "«private-key-masked»")
	s = gcpKeyRE.ReplaceAllString(s, `"private_key":"«masked»"`)
	for _, rule := range leakRules {
		s = rule.re.ReplaceAllString(s, "«"+rule.kind+"-masked»")
	}
	return s
}

// clampLog bounds a log field and strips NUL (keeps normal whitespace/newlines).
func clampLog(s string) string {
	if len(s) > maxVerifyLogLen {
		s = s[:maxVerifyLogLen]
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// clampLine bounds a single-line string and strips control chars (a command
// echoed back into a step result).
func clampLine(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	var b strings.Builder
	for _, r := range s {
		if r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
