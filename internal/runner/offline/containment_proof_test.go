// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestMissingIdentityContainmentLeavesAnUnprovenContainerAlone(t *testing.T) {
	for _, kind := range []types.RunnerActionKind{types.RunnerActionKill, types.RunnerActionStopProxy} {
		t.Run(string(kind), func(t *testing.T) {
			m, sub, _, _ := managerFixture(t)
			a := orphanAction(m, kind)
			sub.foreign = errors.New("container carries no Wardyn labels")
			if _, err := m.apply(t.Context(), a); !errors.Is(err, sub.foreign) || sub.kills+sub.stops != 0 {
				t.Fatalf("contained a container the runner cannot prove it created: %v", err)
			}
			if _, bound := m.state.Runs[a.RunID]; bound {
				t.Fatal("an unproven container gained a containment identity")
			}
			if sub.provedFor != m.runnerID.String() {
				t.Fatalf("proved for runner %q", sub.provedFor)
			}
			sub.foreign = nil
			if result, err := m.apply(t.Context(), a); err != nil || result.Outcome != "success" || sub.kills+sub.stops != 1 {
				t.Fatalf("this runner's lost sandbox was not contained: %+v %v", result, err)
			}
		})
	}
}
