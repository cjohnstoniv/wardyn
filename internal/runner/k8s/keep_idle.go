// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"fmt"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// labelAgent is the run's agent id, which dispatch sets in SandboxSpec.Labels, so the agent pod and its Sandbox's template
// carry it.
const labelAgent = "wardyn.agent"

// reviveHarnesses are the agents whose kept runs are revived on Kubernetes: their images have a revive mode of their own
// and take it on a second boot (booted_before). A run of any other agent is not kept and ends as before.
var reviveHarnesses = []string{"claude-code", "codex-cli"}

// keepHarnessRefusal is "" when a run of agent can be kept and revived, else the reason it is not.
func keepHarnessRefusal(agent string) string {
	if slices.Contains(reviveHarnesses, agent) {
		return ""
	}
	return fmt.Sprintf("k8s: this run is not kept: only claude-code and codex-cli runs are revived on Kubernetes, and its agent is %q", agent)
}

// keepIdleScript is a kept run's main command. The home folder is not kept, so the image's own booted marker is lost when
// the pod is made again. A marker in the kept work folder restores it before agent-run --idle, and the image then takes
// its own revive branch (booted_before), as it does on Docker. The agent starts only once the marker this boot needs is
// in place: the first boot's work-folder marker, without which a revive would run the first task again, or a later
// boot's home marker, without which this boot would. Anything else at the marker's path is refused.
func keepIdleScript(runID string) []string {
	w := runner.ScratchWorkPath + "/.wardyn"
	m := w + "/booted-" + runID
	return []string{"sh", "-c", `m=` + m + `; h="${HOME:-/home/agent}"; ` +
		`if [ -f "$m" ]; then mkdir -p "$h/.wardyn" && true > "$h/.wardyn/booted" || { echo "wardyn: cannot restore $h/.wardyn/booted for this kept run" >&2; exit 1; }; ` +
		`elif [ -e "$m" ] || [ -L "$m" ]; then echo "wardyn: $m is not a file" >&2; exit 1; ` +
		`else mkdir -p ` + w + ` && true > "$m" || { echo "wardyn: cannot write $m, so a revive would run the first task again" >&2; exit 1; }; fi; ` +
		`exec agent-run --idle`}
}
