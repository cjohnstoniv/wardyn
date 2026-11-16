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

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A workload's policy pin is the launch policy digest every launch of it must resolve to. stepPolicyPin takes
// the digest after the last fold step that writes the run's policy, so it covers what the policy step resolved
// (the stored policy or the default, the inline overlay and overrides, and the owner's ceiling, walls and
// boundary) and what the steps after it add: the workspace seed, an Azure DevOps narrowing, the workspace
// requirements, the direct GitHub hosts, the components, the size asked and the idle stop, and the tool
// posture. It is taken before anything is minted or written and before the create answers, so a launch off
// its pin is refused and the run it would replace is never touched. The run's own policy_digest cannot be the
// pin: dispatch takes it after the create has answered (recordRunPolicyDigest), over what dispatch adds from
// state it reads then, such as the site configuration and the run's credentials.

// workloadPolicyPinPattern is a pin's form: a lowercase hex SHA-256, as RunPolicySpec.Digest writes it.
var workloadPolicyPinPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const (
	workloadPolicyPinMismatchMsg = "This workload is pinned to launch policy digest %s, but its policy now resolves to %s. " +
		"Pin the new digest to accept the change."
	workloadPolicyPinInvalidMsg = "A policy pin is a launch policy digest: 64 lowercase hex characters, as a workload preview's launch.policy_digest shows it."
)

// policyPinKey carries a workload launch's pin into the create fold: launchClaimed and the preview put it on
// the create request, and queueAuthority keeps it for a launch that waits in its pool's queue.
type policyPinKey struct{}

// policyPinCheck is one launch's pin: want is the workload's pin, "" for none, and got the launch policy
// digest stepPolicyPin took, "" until it ran, which the preview reports. size is the size the launch gets
// before any runner's per-run limit cuts it, as unboundSize found it at placement.
type policyPinCheck struct {
	want, got string
	size      types.RunnerAmounts
}

func withPolicyPin(ctx context.Context, want string) (context.Context, *policyPinCheck) {
	p := &policyPinCheck{want: want}
	return context.WithValue(ctx, policyPinKey{}, p), p
}

// policyPinOf is the pin ctx carries, nil outside a workload's launch.
func policyPinOf(ctx context.Context) *policyPinCheck {
	p, _ := ctx.Value(policyPinKey{}).(*policyPinCheck)
	return p
}

// stepPolicyPin holds a workload's launch to its policy pin at every door, after stepNoNetwork. stepToolPolicy
// is the last step every door runs that writes the spec, so the digest holds each of them; the dry doors'
// stepDerivedEgress, which runs after this step, adds what create adds after the mint.
func (s *Server) stepPolicyPin(f *runFold, _ *foldRecorder) bool {
	p := policyPinOf(f.r.Context())
	if p == nil {
		return true
	}
	p.got = launchPolicyDigest(f.spec, f.wsRefs, f.req.Repo, f.ceiling.Spec.AllowedDomains, p.size)
	return !p.refusal().write(s, f.w, f.r)
}

// refusal is the answer to a launch whose digest is p.got: none when it carries no pin or resolved to it.
func (p *policyPinCheck) refusal() *runRefusal {
	if p.want == "" || p.want == p.got {
		return nil
	}
	return runError(http.StatusConflict, reasonWorkloadPolicyPinMismatch, fmt.Sprintf(workloadPolicyPinMismatchMsg, p.want, p.got))
}

// launchPolicyDigest is a launch's policy digest, one value at every door for one launch. It is spec, the policy
// the fold resolved, with the egress of wsRefs, the run's workspaces, taken in at every door (the dry doors' stepPreviewEgress holds it
// by now; create's unionRunEgress adds it after the mint). Its CPU, memory and main drive are size, the size the
// launch gets before any runner cuts it: what a runner with room gives is the fleet's state, not the launch's,
// and the dry doors' spec holds the main drive a runner cut. The doors add the same hosts in another order, so
// the host lists are compared as sets. spec is not changed. It takes the fold's parts, not the fold: only a
// runFoldSteps step may take a runFold (TestPreflightMirrorsLaunchGates).
func launchPolicyDigest(spec types.RunPolicySpec, wsRefs []types.Workspace, repo string, trusted []string, size types.RunnerAmounts) string {
	v := auditablePolicy(spec)
	unionPreviewWorkspaceEgress(&v, wsRefs, runRepoLocators(v, repo), trusted)
	rl := types.ResourceLimits{}
	if v.Resources != nil {
		rl = *v.Resources
	}
	rl.CPUMillis, rl.MemoryMiB, rl.DiskMiB = int(size.CPUMillis), int(size.MemoryMiB), int(size.DiskMiB)
	v.Resources = &rl // a fresh block: v.Resources may still be the fold's own
	v.AllowedDomains, v.DeniedDomains = pinHostSet(v.AllowedDomains), pinHostSet(v.DeniedDomains)
	return v.Digest()
}

// pinHostSet is hosts lowercased and trimmed, sorted, each once and none empty, as unionDomains keys them.
func pinHostSet(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, h)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// handlePutWorkloadPolicyPin is PUT /workloads/{name}/policy-pin: pin the caller's own workload of that name
// to a launch policy digest, the one a preview of its launch reports (launch.policy_digest). As for the
// apply, there is no ?owner=: the caller is the owner.
func (s *Server) handlePutWorkloadPolicyPin(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	var req client.WorkloadPolicyPinRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !workloadPolicyPinPattern.MatchString(req.Digest) {
		writeErrorReason(w, http.StatusBadRequest, reasonWorkloadPolicyPinInvalid, workloadPolicyPinInvalidMsg)
		return
	}
	s.setWorkloadPolicyPin(w, r, ws, req.Digest)
}

// handleDeleteWorkloadPolicyPin is DELETE /workloads/{name}/policy-pin: the caller's own workload launches
// with whatever policy it resolves to again.
func (s *Server) handleDeleteWorkloadPolicyPin(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workloadStore(w)
	if !ok {
		return
	}
	s.setWorkloadPolicyPin(w, r, ws, "")
}

// setWorkloadPolicyPin writes pin on the caller's workload of the route's name, records a change as
// workload.policy_pin.set, and answers the workload as it now reads. A launch already claimed keeps the pin
// it was claimed with. A ?owner= naming anyone but the caller reaches no workload: the route never writes
// the caller's own workload for a call that named another's.
func (s *Server) setWorkloadPolicyPin(w http.ResponseWriter, r *http.Request, ws store.WorkloadStore, pin string) {
	if o := r.URL.Query().Get("owner"); o != "" && o != principalFromRequest(r) {
		writeErrorReason(w, http.StatusNotFound, reasonWorkloadNotFound, workloadNotFoundMsg)
		return
	}
	ps, ok := ws.(store.WorkloadPolicyPins)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonWorkloadsUnavailable, "workloads require the Postgres store backend")
		return
	}
	ctx := r.Context()
	wl, prev, err := ps.SetWorkloadPolicyPin(ctx, principalFromRequest(r), chi.URLParam(r, "name"), pin)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonWorkloadNotFound, workloadNotFoundMsg)
		return
	case err != nil:
		writeServerError(w, r, "pin workload policy", err)
		return
	}
	if prev != pin {
		actorType, actor := actorFromRequest(r)
		s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "workload.policy_pin.set", workloadTarget(wl), "success", mustJSON(map[string]any{
			"name": wl.Name, "owner": wl.Owner, "generation": wl.Generation, "policy_pin": pin, "previous": prev,
		})))
	}
	v, err := ws.GetWorkload(ctx, wl.Owner, wl.Name)
	if err != nil {
		v = store.WorkloadView{Workload: wl} // deleted under the pin: answer what was recorded
	}
	writeJSON(w, http.StatusOK, s.workloadReadFor(r, v))
}
