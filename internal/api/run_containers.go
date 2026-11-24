// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	maxRunContainers       = 4
	maxRunContainerPorts   = 8
	maxRunContainerCommand = 32
	maxRunContainerArg     = 4096
	// maxRunContainerCPUMillis and maxRunContainerMemoryMiB bound one container's ask far above any run's size
	// and far below an overflow (1 << 20 millicores, 16 TiB), so no arithmetic on them can wrap.
	maxRunContainerCPUMillis = 1 << 20
	maxRunContainerMemoryMiB = 1 << 24
	// agentFloorCPUMillis and agentFloorMemoryMiB are what the agent keeps of the run's size.
	agentFloorCPUMillis = 250
	agentFloorMemoryMiB = 256
)

// runContainerName is a DNS label of at most 40 characters.
var runContainerName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

// containersShapeRefusal holds the request's extra containers to their shape. It reads only the request,
// so a template and a preset are held to it too. A request with none passes.
func containersShapeRefusal(req createRunRequest) *runRefusal {
	if len(req.Containers) == 0 {
		return nil
	}
	if types.MachineKind(req.MachineKind).IsVM() {
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
			"containers: extra containers run in a container sandbox, not a virtual machine.")
	}
	if len(req.Containers) > maxRunContainers {
		return containersInvalid("containers", "containers: at most %d extra containers", maxRunContainers)
	}
	names, ports := map[string]bool{}, map[int]bool{}
	for i, c := range req.Containers {
		at := fmt.Sprintf("containers[%d]", i)
		switch {
		case !runContainerName.MatchString(c.Name) || c.Name == "agent" ||
			strings.HasPrefix(c.Name, "wardyn-") || strings.HasPrefix(c.Name, "stage-"):
			return containersInvalid(at+".name", "%s.name: lower-case letters, digits and dashes, at most 40, "+
				"not \"agent\" and not starting with \"wardyn-\" or \"stage-\"", at)
		case names[c.Name]:
			return containersInvalid(at+".name", "%s.name: %q names two containers", at, c.Name)
		case !containerImageOK(c.Image):
			return containerImageRefusal(i)
		case c.Resources.CPUMillis <= 0 || c.Resources.MemoryMiB <= 0 ||
			c.Resources.CPUMillis > maxRunContainerCPUMillis || c.Resources.MemoryMiB > maxRunContainerMemoryMiB:
			return containersInvalid(at+".resources", "%s.resources: cpu_millis (1 to %d) and memory_mib (1 to %d) are both required",
				at, maxRunContainerCPUMillis, maxRunContainerMemoryMiB)
		case len(c.Command) > maxRunContainerCommand || slices.ContainsFunc(c.Command, func(a string) bool {
			return a == "" || len(a) > maxRunContainerArg || strings.ContainsRune(a, 0)
		}):
			return containersInvalid(at+".command", "%s.command: at most %d non-empty arguments of at most %d bytes",
				at, maxRunContainerCommand, maxRunContainerArg)
		case len(c.Ports) > maxRunContainerPorts:
			return containersInvalid(at+".ports", "%s.ports: at most %d ports", at, maxRunContainerPorts)
		}
		names[c.Name] = true
		for _, p := range c.Ports {
			if p < 1024 || p > 65535 || ports[p] {
				return containersInvalid(at+".ports", "%s.ports: each port is 1024 to 65535 and used by one container only", at)
			}
			ports[p] = true
		}
	}
	return nil
}

func containersInvalid(field, format string, args ...any) *runRefusal {
	ref := runError(http.StatusBadRequest, reasonRunContainersInvalid, fmt.Sprintf(format, args...))
	ref.body.Field = field
	return ref
}

// containerImageOK is an extra's image shape: one bounded ref with no control character, pinned by digest.
func containerImageOK(ref string) bool {
	return len(ref) <= maxRunOverrideValueLen && controlCharFree(ref) && prewrappedPinned(ref)
}

// containerImageRefusal is the shape refusal of containers[i].image. runRequestGovernance answers it too, before
// its sentences echo the ref, since that door runs before the shape checks (runs_create_validate.go:144).
func containerImageRefusal(i int) *runRefusal {
	return containersInvalid(fmt.Sprintf("containers[%d].image", i),
		"containers[%d].image: one image ref of at most %d bytes, pinned by digest (<repository>@sha256:<64 hex>)", i, maxRunOverrideValueLen)
}

// containersBudgetRefusal refuses extras that leave the agent less than its floor of the run's applied size. It
// takes each container out of what is left and refuses the first that does not fit, so it never adds request
// values together and nothing can wrap. A run with no applied size has nothing to share.
func containersBudgetRefusal(containers []client.RunContainer, applied *types.ResourceLimits) *runRefusal {
	if len(containers) == 0 {
		return nil
	}
	var cpu, mem int // what is left for the agent
	if applied != nil {
		cpu, mem = applied.CPUMillis, applied.MemoryMiB
	}
	for _, c := range containers {
		if c.Resources.CPUMillis > cpu || c.Resources.MemoryMiB > mem {
			return containersOverBudget(fmt.Sprintf("The extra container %q asks for %d millicores and %d MiB, and %d and %d of this run's size are left.",
				c.Name, c.Resources.CPUMillis, c.Resources.MemoryMiB, cpu, mem))
		}
		cpu, mem = cpu-c.Resources.CPUMillis, mem-c.Resources.MemoryMiB
	}
	if cpu < agentFloorCPUMillis || mem < agentFloorMemoryMiB {
		return containersOverBudget(fmt.Sprintf("The extra containers leave the agent %d millicores and %d MiB of this run's size; it keeps at least %d and %d.",
			cpu, mem, agentFloorCPUMillis, agentFloorMemoryMiB))
	}
	return nil
}

func containersOverBudget(msg string) *runRefusal {
	ref := runError(http.StatusUnprocessableEntity, reasonRunContainersOverBudget, msg+" Lower the containers' resources or raise the run's.")
	ref.body.Field = "containers"
	return ref
}

// stepContainersBudget holds the extras to the run's applied size (after stepResources).
func (s *Server) stepContainersBudget(f *runFold, _ *foldRecorder) bool {
	return !containersBudgetRefusal(f.req.Containers, f.spec.Resources).write(s, f.w, f.r)
}

// stepContainerAdmission holds each extra's image to image admission as stepImageAdmission holds the run's,
// and freezes the run's containers with their verdicts.
func (s *Server) stepContainerAdmission(f *runFold, _ *foldRecorder) bool {
	if len(f.req.Containers) == 0 {
		return true
	}
	out := make([]types.RunExtraContainer, 0, len(f.req.Containers))
	for i, c := range f.req.Containers {
		v, warning, refusal := s.imageAdmissionOf(f.r, c.Image, types.MachineKind(f.req.MachineKind),
			f.ceiling.Limits.RequiredImageSigners, f.mode == foldCreate)
		if refusal != nil {
			refusal.body.Field = fmt.Sprintf("containers[%d].image", i)
			refusal.write(s, f.w, f.r)
			return false
		}
		image := c.Image
		if v != nil && v.PinnedRef != "" {
			image = v.PinnedRef
		}
		out = append(out, types.RunExtraContainer{Name: c.Name, Image: image, Command: slices.Clone(c.Command),
			Ports: slices.Clone(c.Ports), CPUMillis: c.Resources.CPUMillis, MemoryMiB: c.Resources.MemoryMiB, ImageAdmission: v})
		if warning != "" {
			f.policyWarns = append(f.policyWarns, warning)
		}
	}
	f.containers = out
	return true
}

// extraContainersUnsupportedMsg is the placement_capability sentence for a placement that cannot run extras.
const extraContainersUnsupportedMsg = "This run's sandbox (%s) cannot run extra containers. In this release they run on Kubernetes only."

// stepContainersPlacement refuses extras on a placement that cannot run them (after stepPlacementCapability).
func (s *Server) stepContainersPlacement(f *runFold, _ *foldRecorder) bool {
	return !s.containersPlacementRefusal(f.r.Context(), *f.req, f.place).write(s, f.w, f.r)
}

// containersPlacementRefusal is placement_capability, field containers, when wardynd's own substrate, or any
// runner the run could land on, does not offer Capabilities.ExtraContainers. It reads the placement as
// placementCapabilityRefusalFor does. A request with none passes.
func (s *Server) containersPlacementRefusal(ctx context.Context, req createRunRequest, place runPlacement) *runRefusal {
	if len(req.Containers) == 0 {
		return nil
	}
	runners := []uuid.UUID{requestRunnerID(req)}
	p := requestPlacement(req)
	if place.pooled {
		p, runners = place.placement, place.eligibleRunners()
	}
	if p != types.PlacementLocal {
		runners = []uuid.UUID{uuid.Nil} // wardynd's own substrate: one answer, read without a runner id
	}
	for _, id := range runners {
		pc, err := s.placementCapabilities(ctx, p, id)
		var ref *placement.Refusal
		switch {
		case errors.Is(err, errNoRemoteExecutor): // -runner none: nothing here will run the extras
			return placementRunRefusal(&placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: "containers"},
				fmt.Sprintf(extraContainersUnsupportedMsg, "none"))
		case errors.As(err, &ref):
			return placementRunRefusal(ref, ref.Detail)
		case err != nil:
			return runServerError("read runner capabilities", err)
		case !pc.ExtraContainers:
			return placementRunRefusal(&placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: "containers"},
				fmt.Sprintf(extraContainersUnsupportedMsg, pc.Driver))
		}
	}
	return nil
}

// stepContainersRecording refuses extras where the organisation requires recording: an organisation's or the run's
// pool's `recording: required`, or an organisation level locked above off. What the agent and an extra exchange over
// loopback is in neither the recording nor the proxy's audit. A governance profile's own recording level does not
// refuse them. A read that fails is a server error (createRecordingChoice), so this fails closed.
func (s *Server) stepContainersRecording(f *runFold, _ *foldRecorder) bool {
	if len(f.req.Containers) == 0 {
		return true
	}
	choice, refusal := s.createRecordingChoice(f.r.Context(), f.r, f.spec.Recording, f.place, f.ceiling, f.policyID == nil)
	if refusal.write(s, f.w, f.r) {
		return false
	}
	locked := choice.Org != nil && choice.Org.Locked && choice.Org.OrgLevel().Rank() > types.RecordingOff.Rank()
	if !choice.Required && !locked {
		return true
	}
	ref := runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
		"Extra containers are not available where your organisation requires recording: what the agent and an extra container exchange inside the sandbox is not recorded.")
	ref.body.Field = "containers"
	return !ref.write(s, f.w, f.r)
}

// auditContainers is the run.create row's view of the extras: what runs, how big, and its verdict. Never the
// command: argv can carry a pasted secret as an env value can.
func auditContainers(cs []types.RunExtraContainer) []map[string]any {
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{"name": c.Name, "image": c.Image, "cpu_millis": c.CPUMillis,
			"memory_mib": c.MemoryMiB, "ports": c.Ports, "image_admission": c.ImageAdmission})
	}
	return out
}

type runContainersReader interface {
	RunContainers(ctx context.Context, runID uuid.UUID) ([]types.RunExtraContainer, error)
}

// dispatchContainers is the run's extras as the runner takes them. A store that cannot read them answers none:
// only the Postgres store creates runs, and it can.
func (s *Server) dispatchContainers(ctx context.Context, runID uuid.UUID) ([]runner.ExtraContainer, error) {
	rd, ok := s.cfg.Store.(runContainersReader)
	if !ok {
		return nil, nil
	}
	cs, err := rd.RunContainers(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]runner.ExtraContainer, 0, len(cs))
	for _, c := range cs {
		out = append(out, runner.ExtraContainer{Name: c.Name, Image: c.Image, Command: c.Command, Ports: c.Ports,
			CPUMillis: int64(c.CPUMillis), MemoryMiB: int64(c.MemoryMiB)})
	}
	return out, nil
}
