// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestMissingIdentityContainmentLeavesAnUnprovenContainerAlone(t *testing.T) {
	for _, kind := range []types.RunnerActionKind{types.RunnerActionKill, types.RunnerActionStopProxy} {
		t.Run(string(kind), func(t *testing.T) {
			m, sub, _, _ := managerFixture(t)
			a := orphanAction(m, kind)
			sub.foreign = fmt.Errorf("container carries no Wardyn labels: %w", runner.ErrNotRunnerSandbox)
			result, err := m.apply(t.Context(), a)
			if err != nil || result.Outcome != types.RunnerActionRefused || !strings.Contains(result.Reason, sub.foreign.Error()) || sub.kills+sub.stops != 0 {
				t.Fatalf("an unproven container: %+v %v", result, err)
			}
			if _, bound := m.state.Runs[a.RunID]; bound {
				t.Fatal("an unproven container gained a containment identity")
			}
			if sub.provedFor != m.runnerID.String() {
				t.Fatalf("proved for runner %q", sub.provedFor)
			}
			// The refusal is terminal: proving the container later changes nothing for this action.
			sub.foreign = nil
			if replay, err := m.apply(t.Context(), a); err != nil || replay != result || sub.kills+sub.stops != 0 {
				t.Fatalf("refusal replay: %+v %v", replay, err)
			}
			b := orphanAction(m, kind)
			if result, err := m.apply(t.Context(), b); err != nil || result.Outcome != "success" || sub.kills+sub.stops != 1 {
				t.Fatalf("this runner's lost sandbox was not contained: %+v %v", result, err)
			}
		})
	}
}

func TestHungLostIdentityProofTimesOutWithoutHoldingTheManager(t *testing.T) {
	m, sub, _, _ := managerFixture(t)
	m.proofTimeout = 2 * time.Second
	sub.hung = make(chan struct{}, 1)
	a := orphanAction(m, types.RunnerActionKill)
	done := make(chan error, 1)
	go func() {
		_, err := m.apply(context.Background(), a)
		done <- err
	}()
	<-sub.hung
	other := make(chan bool, 1)
	go func() { other <- m.HoldsSandbox(a.RunID.String()) }()
	select {
	case <-other:
	case <-done:
		t.Fatal("the proof answered before its timeout")
	case <-time.After(time.Second):
		t.Fatal("a hung engine inspect held up another manager call")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || sub.kills != 0 {
			t.Fatalf("hung proof: kills=%d err=%v", sub.kills, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a hung engine inspect never timed out")
	}
	if _, bound := m.state.Runs[a.RunID]; bound {
		t.Fatal("a timed-out proof left a containment identity")
	}
}

// An engine that could not answer is not a negative proof: the action is left
// for the next pass, and once the engine answers the runner's own lost sandbox
// is contained.
func TestLostIdentityProofRetriesWhenTheEngineCannotAnswer(t *testing.T) {
	m, sub, _, _ := managerFixture(t)
	a := orphanAction(m, types.RunnerActionKill)
	sub.foreign = errors.New("docker: inspect: Cannot connect to the Docker daemon. Is the docker daemon running?")
	result, err := m.apply(t.Context(), a)
	if err == nil || result.Outcome != "" || sub.kills != 0 {
		t.Fatalf("an engine error ended the action: %+v %v", result, err)
	}
	if _, recorded := m.state.Actions[a.ID]; recorded {
		t.Fatal("an engine error left a durable answer")
	}
	sub.foreign = nil
	if result, err := m.apply(t.Context(), a); err != nil || result.Outcome != "success" || sub.kills != 1 {
		t.Fatalf("the engine came back but the sandbox was not contained: %+v %v", result, err)
	}
}

// However the engine's error text escapes, the refused result fits the org's
// action_result bound, so it is never refused and replayed on every reconnect.
func TestRefusedResultFitsTheOrgBound(t *testing.T) {
	m, sub, _, _ := managerFixture(t)
	a := orphanAction(m, types.RunnerActionKill)
	sub.foreign = fmt.Errorf("Error response from daemon: %s: %w", strings.Repeat("<a>&\x01", 120), runner.ErrNotRunnerSandbox)
	result, err := m.apply(t.Context(), a)
	if err != nil || result.Outcome != types.RunnerActionRefused || result.Reason == "" || len(result.Reason) > runnerwire.MaxRefusalReason {
		t.Fatalf("refusal: %+v %v", result, err)
	}
	b, err := json.Marshal(result)
	if err != nil || len(b) > runnerwire.MaxActionResultBytes {
		t.Fatalf("the encoded result is %d bytes, over the org's %d", len(b), runnerwire.MaxActionResultBytes)
	}
}
