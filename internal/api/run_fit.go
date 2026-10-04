// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// quotaNearFullPct is where a run that is still admitted is told it nearly fills a quota: at or
// over this share of the quota's hard limit, counting what the run itself adds.
const quotaNearFullPct = 90

// runFitVerdict is what the substrate's fit check says about one run: a refusal when a hard
// quota cannot hold it, and advisories otherwise.
type runFitVerdict struct {
	refusal  string
	warnings []string
}

// refuseRunFit is the ResourceQuota gate create and preflight share: a run that cannot fit the
// runs namespace's quota is refused 422 before the identity mint, so it leaves no run row and no
// sandbox, and the advisories ride the response. It reports true once it has answered.
//
// It claims no more than the quota objects say. They are read as they stand, nothing is
// reserved, so a concurrent run can still take the room (the quota's own admission then refuses
// the pod, and the run fails to start); and the node-size advisory compares requests to node
// size, never to free capacity, because other namespaces' pods are invisible to this role.
func (s *Server) refuseRunFit(w http.ResponseWriter, r *http.Request, spec types.RunPolicySpec) (warnings []string, refused bool) {
	v := s.runFit(r.Context(), spec)
	if v.refusal != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonNamespaceQuotaExceeded, v.refusal)
		return nil, true
	}
	return v.warnings, false
}

// runFitSpec is the spec the fit check is asked about: the policy with the disk dispatch will
// settle on (the org default filled in, the org and profile maximums applied), so a hard
// ephemeral-storage quota is judged the same at both doors. It works on a copy: the fill must
// never reach a launch spec (runner.Resources.DiskMiBFilled carries its provenance).
func (s *Server) runFitSpec(ctx context.Context, spec types.RunPolicySpec, ceiling governanceCeiling) types.RunPolicySpec {
	profileMax := 0
	if ceiling.Profile != nil && !s.runUngoverned(ctx) {
		profileMax = ceiling.Limits.MaxEphemeralDiskMiB
	}
	s.previewEphemeralDisk(ctx, &spec, profileMax)
	return spec
}

// runFit asks the substrate whether a run of the spec's size fits. A deployment whose runner has no
// fit to check (docker, or no runner) has nothing to say. A check that errors is reported as an
// unreadable quota: the quota is enforced when the run starts either way.
func (s *Server) runFit(ctx context.Context, spec types.RunPolicySpec) runFitVerdict {
	fc, ok := s.cfg.Runner.(runner.FitChecker)
	if !ok {
		return runFitVerdict{}
	}
	fit, err := fc.CheckFit(ctx, resourceLimitsToRunner(spec.Resources))
	if errors.Is(err, runner.ErrFitUnsupported) {
		return runFitVerdict{}
	}
	if err != nil {
		fit = runner.Fit{Quotas: runner.ReadUnavailable, Nodes: runner.ReadSkipped}
	}
	return judgeFit(fit)
}

// judgeFit turns a fit answer into sentences. The first quota that cannot hold the run decides
// the refusal; with none, every quota the run nearly fills and every unreadable read is an
// advisory. Empty, forbidden and unavailable stay three different results: an empty list says
// nothing, the other two each say why the quotas were not checked.
func judgeFit(fit runner.Fit) runFitVerdict {
	var v runFitVerdict
	switch fit.Quotas {
	case runner.ReadForbidden:
		v.warnings = append(v.warnings, quotaUnreadableWarning("permission denied"))
	case runner.ReadUnavailable:
		v.warnings = append(v.warnings, quotaUnreadableWarning("unavailable"))
	}
	for _, q := range fit.Quota {
		if short := breachedAxes(q.Axes); len(short) > 0 {
			v.refusal = quotaExceededReason(q.Name, short)
			return v
		}
		if pct, ok := nearFullPct(q.Axes); ok {
			v.warnings = append(v.warnings, quotaNearFullWarning(q.Name, pct, q.Axes))
		}
	}
	switch fit.Nodes {
	case runner.ReadForbidden:
		v.warnings = append(v.warnings, nodeUnreadableWarning("permission denied"))
	case runner.ReadUnavailable:
		v.warnings = append(v.warnings, nodeUnreadableWarning("unavailable"))
	}
	if n := fit.NodeShortfall; n != nil {
		v.warnings = append(v.warnings, nodeFitWarning(n))
	}
	return v
}

// breachedAxes are the quota's limits the run asks more of than is left.
func breachedAxes(axes []runner.QuotaAxis) []runner.QuotaAxis {
	var out []runner.QuotaAxis
	for _, a := range axes {
		if a.Need > a.Left {
			out = append(out, a)
		}
	}
	return out
}

// nearFullPct is the highest share, over the quota's limits the run touches, that the quota would
// stand at once the run is in, and whether it reaches quotaNearFullPct. The test is integer
// arithmetic, so a run that fills a quota to 89.9% is never rounded up into a warning.
func nearFullPct(axes []runner.QuotaAxis) (int, bool) {
	top, warn := 0, false
	for _, a := range axes {
		if a.Hard <= 0 {
			continue
		}
		after := a.Hard - a.Left + a.Need
		if after*100 >= a.Hard*quotaNearFullPct {
			warn = true
		}
		top = max(top, int(after*100/a.Hard))
	}
	return top, warn
}

// The five sentences below are the console's canon (mock packet M1, section 3) and the console
// adds no string of its own: it shows the server's text. {need} and {left} count both of the
// run's pods.

func quotaExceededReason(quota string, short []runner.QuotaAxis) string {
	return fmt.Sprintf("this run needs %s, more than quota %s has left (%s). Stop a run, or ask your admin to raise the quota.",
		quotaAmounts(short, func(a runner.QuotaAxis) int64 { return a.Need }), quota,
		quotaAmounts(short, func(a runner.QuotaAxis) int64 { return a.Left }))
}

func quotaNearFullWarning(quota string, pct int, axes []runner.QuotaAxis) string {
	return fmt.Sprintf("this run would fill quota %s to %d%% (%s left after it) — later runs may be refused.",
		quota, pct, quotaAmounts(axes, func(a runner.QuotaAxis) int64 { return a.Left - a.Need }))
}

func nodeFitWarning(n *runner.NodeShortfall) string {
	need := []string{fitAmount("cpu", n.CPUMillis), fitAmount("memory", n.MemoryBytes)}
	return fmt.Sprintf("no node this run may be placed on is large enough for %s. It may wait unscheduled until one is.",
		strings.Join(need, ", "))
}

func quotaUnreadableWarning(why string) string {
	return fmt.Sprintf("couldn't read this namespace's quotas (%s) — they are still enforced when the run starts.", why)
}

func nodeUnreadableWarning(why string) string {
	return fmt.Sprintf("couldn't read node sizes (%s) — the node-size check was skipped.", why)
}

// quotaAmounts renders one quantity per axis, "2 CPU, 4Gi memory". A limit axis says so ("4 CPU
// limit"), so a quota that bounds both requests and limits does not print two unlabelled CPUs.
func quotaAmounts(axes []runner.QuotaAxis, pick func(runner.QuotaAxis) int64) string {
	parts := make([]string, 0, len(axes))
	for _, a := range axes {
		parts = append(parts, fitAmount(a.Key, pick(a)))
	}
	return strings.Join(parts, ", ")
}

// fitAmount is one quantity in the console's words. The key is a quota key ("requests.cpu",
// "limits.memory", "pods", ...); CPU arrives in millicores, memory and storage in bytes.
func fitAmount(key string, v int64) string {
	base, limit := strings.CutPrefix(key, "limits.")
	if !limit {
		base = strings.TrimPrefix(key, "requests.")
	}
	suffix := ""
	if limit {
		suffix = " limit"
	}
	switch base {
	case "cpu":
		return strconv.FormatFloat(float64(v)/1000, 'f', -1, 64) + " CPU" + suffix
	case "memory", "ephemeral-storage":
		return binaryBytes(max(v, 0)) + " " + strings.ReplaceAll(base, "-", " ") + suffix
	case "pods", "count/pods":
		if v == 1 {
			return "1 pod"
		}
		return strconv.FormatInt(v, 10) + " pods"
	}
	return strconv.FormatInt(v, 10) + " " + base
}

// binaryBytes is a byte count in the largest binary unit that divides it exactly ("4Gi",
// "512Mi"), the way a quota is written, and the plain count when none does.
func binaryBytes(v int64) string {
	for _, u := range []struct {
		size int64
		name string
	}{{1 << 30, "Gi"}, {1 << 20, "Mi"}, {1 << 10, "Ki"}} {
		if v >= u.size && v%u.size == 0 {
			return strconv.FormatInt(v/u.size, 10) + u.name
		}
	}
	return strconv.FormatInt(v, 10)
}
