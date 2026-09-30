// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Bedrock data-plane refusals after dispatch (the proxy's upstream_fault,
// internal/egress/proxy/bedrock_fault.go). An agent whose model call AWS
// refuses exits non-zero, and a FAILED-by-exit-code run carries no hint of its
// own — so the owner saw "exit 1" and nothing about the AWS policy or quota
// that caused it.
//
// The sentence is written to failure_hint AS THE DECISION ARRIVES, not when the
// run ends: the completion watcher can be on another replica, or the daemon can
// restart in between, and failure_hint is the one durable per-run line a
// reader already sees. projectFailureHint keeps it off every run that did not
// end FAILED, and a "recovered" row clears it, so a throttle the SDK's retry
// cleared never outlives that retry.
var bedrockFaultHints = map[string]string{
	"AccessDeniedException": "Amazon Bedrock refused the model call (AccessDeniedException): a policy denies it — " +
		"an AWS Organizations service control policy or an IAM policy on the role — or model access is not " +
		"enabled for this account. Ask your AWS administrator to allow bedrock:InvokeModel for this model.",
	"ThrottlingException": "Amazon Bedrock is throttling this model (ThrottlingException): the account's request " +
		"quota was still exhausted after the agent's own retries. Try again later, or ask your AWS administrator " +
		"for a higher Bedrock quota.",
}

// noteBedrockDataPlaneFault records fault (a class, or "recovered") on the
// run's failure_hint. Best-effort, like every other hint write.
func (s *Server) noteBedrockDataPlaneFault(ctx context.Context, runID uuid.UUID, fault string) {
	setter, ok := s.cfg.Store.(runFailureHintSetter)
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return
	}
	_, ours := bedrockFaultHintSet[run.FailureHint]
	hint, known := bedrockFaultHints[fault]
	switch {
	case fault == "recovered":
		if !ours {
			return // never clear a hint this file did not write
		}
		hint = ""
	case !known:
		return
	case isTerminalRunState(run.State) &&
		!(run.State == types.RunFailed && (run.FailureHint == "" || ours)):
		// A late row for a run that already ended some other way, or whose
		// failure already has its own reason: leave it. A FAILED run with no
		// hint is the watcher having won the race with this row's post.
		return
	}
	if err := setter.SetRunFailureHint(ctx, runID, hint); err != nil {
		slog.WarnContext(ctx, "wardynd: could not persist bedrock data-plane hint",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// bedrockFaultHintSet is bedrockFaultHints' values, for "is this hint ours".
var bedrockFaultHintSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(bedrockFaultHints))
	for _, h := range bedrockFaultHints {
		m[h] = struct{}{}
	}
	return m
}()

// projectFailureHint blanks failure_hint on every run that is not FAILED. Only
// the FAILED transitions wrote it before the Bedrock hint above, which is
// written while the run is still RUNNING and may belong to a run that then
// completed.
func projectFailureHint(runs []types.AgentRun) {
	for i := range runs {
		if runs[i].State != types.RunFailed {
			runs[i].FailureHint = ""
		}
	}
}

// On the per-user AWS SSO lane the proxy tunnels bedrock-runtime opaquely, so
// a model that refuses the agent sends no upstream_fault above and the run
// failed with no hint: the agent's own words, in its recording, were the only
// record (#1280). A run that ends non-zero with no reason of its own is served
// the last recorded line naming a model-access problem, quoted as the agent's,
// never as a cause the server observed.

// modelAccessPhrases are what a harness prints when the model refuses it,
// matched case-insensitively: Claude Code's "It may not exist or you may not
// have access to it", and Bedrock's own AccessDeniedException sentence.
var modelAccessPhrases = []string{"may not have access to it", "don't have access to the model"}

// modelAccessHintFormat is the run's failure_hint quoting the agent's own
// model-access line from its recording (the line's own double quotes become
// single ones, so it cannot close the quote early).
const modelAccessHintFormat = modelAccessHintLead + ": \"%s\". " + modelAccessHintTail

// DRAFT (M2 canon pending) — the same hint for a reader who cannot open the
// recording (projectModelAccessQuote): no recording text.
const modelAccessHintUnquoted = modelAccessHintLead + ". " + modelAccessHintTail

const (
	modelAccessHintLead = "The agent's last output before it exited reported a model-access problem"
	modelAccessHintTail = "Wardyn did not see the model's answer itself; the run's recording has the full output."
)

const (
	modelAccessTailBytes = 16 << 10 // the end of the cast, where an exiting agent's last words are
	modelAccessLineMax   = 300      // the quoted line is cut to this many runes
)

// terminalEscape matches the CSI and OSC sequences a PTY capture carries
// around the text, in their ESC and 8-bit (0x9B, 0x9D) forms, the other
// two-byte ESC sequences, and every other C0 or C1 control but tab, newline
// and carriage return (BEL, a bare ESC).
var terminalEscape = regexp.MustCompile(`(?:\x1b\[|\x9b)[0-?]*[ -/]*[@-~]|(?:\x1b\]|\x9d)[^\x07\x1b\x9c]*(?:\x07|\x1b\\|\x9c)|\x1b[@-Z\\-_]|[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f]`)

// noteModelAccessFromRecording writes the hint above for a run that just
// failed with none. Best-effort, like every other hint write.
func (s *Server) noteModelAccessFromRecording(ctx context.Context, runID uuid.UUID) {
	setter, ok := s.cfg.Store.(runFailureHintSetter)
	if !ok || s.cfg.RecordingStore == nil {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.State != types.RunFailed || run.FailureHint != "" {
		return
	}
	_, tail, err := recording.StatJoined(ctx, s.cfg.RecordingStore, runID.String(), modelAccessTailBytes)
	if err != nil {
		return
	}
	line := lastModelAccessLine(tail)
	if line == "" {
		return
	}
	hint := fmt.Sprintf(modelAccessHintFormat, strings.ReplaceAll(line, `"`, "'"))
	if err := setter.SetRunFailureHint(ctx, runID, hint); err != nil {
		slog.WarnContext(ctx, "wardynd: could not persist model-access hint",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// lastModelAccessLine is the last line of the output events in cast (a tail
// slice: a first line cut short fails to parse and is skipped) that names a
// model-access problem, escape sequences removed and cut to
// modelAccessLineMax runes. "" when none does.
func lastModelAccessLine(cast []byte) string {
	var out strings.Builder
	for _, raw := range bytes.Split(cast, []byte("\n")) {
		var ev []json.RawMessage
		var kind, data string
		if json.Unmarshal(bytes.TrimSpace(raw), &ev) != nil || len(ev) < 3 ||
			json.Unmarshal(ev[1], &kind) != nil || kind != "o" || json.Unmarshal(ev[2], &data) != nil {
			continue
		}
		out.WriteString(data)
	}
	lines := strings.FieldsFunc(terminalEscape.ReplaceAllString(out.String(), ""), func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		// A TUI's tree and bullet glyphs lead the line; they are not its text.
		line := strings.TrimLeftFunc(strings.Join(strings.Fields(lines[i]), " "), func(r rune) bool { return unicode.IsSymbol(r) || unicode.IsSpace(r) })
		lower := strings.ToLower(line)
		for _, p := range modelAccessPhrases {
			if strings.Contains(lower, p) {
				if r := []rune(line); len(r) > modelAccessLineMax {
					line = string(r[:modelAccessLineMax]) + "…"
				}
				return line
			}
		}
	}
	return ""
}

// projectModelAccessQuote keeps the recording's quoted line in a failure hint
// only for a reader who could open that recording, as recordingAuthorizer
// decides it: the run's owner or an operator. A security admin reads every
// run but not its recording (a privacy surface), so they are served the hint
// without the quote.
func (s *Server) projectModelAccessQuote(r *http.Request, runs []types.AgentRun) {
	if s.isOperator(r.Context()) {
		return
	}
	reader := principalFromRequest(r)
	for i := range runs {
		if runs[i].CreatedBy != reader && strings.HasPrefix(runs[i].FailureHint, modelAccessHintLead+": ") {
			runs[i].FailureHint = modelAccessHintUnquoted
		}
	}
}
