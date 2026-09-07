// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBlindCapSuppressionIsAccountedNotSilent pins F066: emitLLMBlindOnce
// bounds its per-run dedup set at maxBlindHosts, and past that cap the
// llm.scan.blind coverage row — the ONLY honest report that an
// inspection-enabled run reached a model host over a tunnel nothing could look
// into (internal/api/healthz.go delegates coverage reporting to this stream) —
// is not emitted. Silence there reads as "no uninspected model tunnel
// happened", which is exactly the inference emitLLMBlindOnce's own doc comment
// ("so audit never implies inspection that did not happen") exists to prevent.
//
// The cap itself is deliberate (an agent under a permissive allowlist can
// enumerate hostnames), so the fix is accounting, not removal: a blind row we
// refuse to emit is an unrecorded decision and lands on the SAME counter the
// sink already keeps for records it could not deliver — the one that feeds the
// periodic `egress.decisions.dropped:<n>` summary and close()'s "closed with N
// dropped records". No new counter, no new audit string.
func TestBlindCapSuppressionIsAccountedNotSilent(t *testing.T) {
	const over = 3
	sink := &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, maxBlindHosts+over+8)}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
		Sink:     sink,
		Resolver: publicResolver{},
		Scanner:  scanEngine(t, "alert", scanTestSecret),
	})

	for i := range maxBlindHosts + over {
		p.emitLLMBlindOnce(fmt.Sprintf("h%d.bedrock-runtime.us-east-1.amazonaws.com", i))
	}

	// The cap still holds: exactly maxBlindHosts rows reach the sink and the
	// dedup map never grows past it.
	if got := len(sink.ch); got != maxBlindHosts {
		t.Fatalf("emitted blind rows = %d, want %d (the cap must still bound the audit stream)", got, maxBlindHosts)
	}
	if got := len(p.blindHosts); got != maxBlindHosts {
		t.Fatalf("blindHosts = %d, want %d (the dedup map must still be bounded)", got, maxBlindHosts)
	}

	// ...and the suppressed ones are ACCOUNTED FOR rather than silent.
	if got := sink.droppedCount(); got != over {
		t.Fatalf("droppedCount() = %d, want %d: the %d blind coverage rows suppressed past the "+
			"cap must be counted where the sink already counts undelivered decisions, or the audit "+
			"stream says 'no uninspected model tunnel' when %d of them happened",
			got, over, over, over)
	}

	// A repeat of an ALREADY-EMITTED host is ordinary dedup, not suppression:
	// its coverage row is on the stream, so it must not inflate the count.
	before := sink.droppedCount()
	p.emitLLMBlindOnce("h0.bedrock-runtime.us-east-1.amazonaws.com")
	if got := sink.droppedCount(); got != before {
		t.Fatalf("droppedCount() = %d after re-emitting an already-reported host, want %d: "+
			"dedup of a host whose coverage row IS on the stream is not a suppression", got, before)
	}
}
