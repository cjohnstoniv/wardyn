// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"net/http"
	"slices"
	"strings"
)

// A bedrock-runtime DATA-PLANE stub, served by the same fake as the two SSO
// services (see the package doc for why one server backs several AWS hosts).
//
// It exists to close the last gap in an end-to-end AWS SSO walk: without it a
// green result proves a role credential was MINTED, never that anything spent
// it. Pointed at by WARDYN_BEDROCK_BASE_URL (which already moves the egress
// entry, the MITM target and the sandbox env together — no new seam), it
// answers both shapes a claude-code run can emit and counts them, so
// /_seen reports "the credential was minted for account X AND a model call was
// made with it".
//
// It is NOT a model: the canned body carries a marker, not an answer. Nothing
// asserts that an agent parsed it — the assertion is that the call arrived.

// bedrockStubMarker is the string the canned answer carries. A walk greps for
// it; a human reading a sandbox's output sees immediately that this was a stub.
const bedrockStubMarker = "wardyn-bedrock-stub"

// bedrockStubBody is the canned Converse-shaped answer. Converse's response
// shape (output.message.content[].text) rather than InvokeModel's because it is
// the one a reader recognises; the stub does not branch on the operation, since
// no assertion anywhere depends on the body being parseable.
const bedrockStubBody = `{"output":{"message":{"role":"assistant","content":[{"text":"` + bedrockStubMarker +
	`"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}`

// handleBedrockRuntime answers every /model/<model-id>/<op> request: Converse,
// ConverseStream, InvokeModel and InvokeModelWithResponseStream all address the
// data plane that way. The model id is read back off the path — it is what
// proves the run carried the operator's configured model (and, via its ARN, the
// pinned account) all the way to the data plane.
func (s *Server) handleBedrockRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	// "/model/<id>/<op>" — split on the LAST slash, because a full
	// inference-profile ARN contains one of its own
	// ("…:inference-profile/us.anthropic.…").
	rest := strings.TrimPrefix(r.URL.Path, "/model/")
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	model := rest[:i]

	s.mu.Lock()
	s.bedrockCalls++
	s.bedrockModel = model
	// …and the CUMULATIVE set, because bedrockModel is last-write-wins and a
	// claude-code run is not one model call. The CLI also drives a small fast
	// model of its own (observed: us.anthropic.claude-haiku-4-5), so whichever
	// call happens to land last decides bedrockModel — and a walk asserting the
	// operator's configured ARN reached the data plane was reading a coin flip.
	// A set answers the question that was actually being asked: did the
	// configured model arrive here AT ALL?
	if !slices.Contains(s.bedrockModels, model) {
		s.bedrockModels = append(s.bedrockModels, model)
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(bedrockStubBody))
}

// There is deliberately NO exported BedrockCalls() accessor beside
// RoleCredentialsSeen: every caller — in-process and on-cluster alike — reads
// the counter through /_seen, which is the one answer a test driving a POD can
// get. A second spelling with no caller is a second thing to keep true.
