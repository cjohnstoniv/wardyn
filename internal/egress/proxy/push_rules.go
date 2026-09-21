// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// Push CONTENT rules: what a brokered git push may contain, as opposed to
// where it may land. The counterpart to confinePush, which is the WHERE.
//
// Three outcomes, and they are the whole feature:
//
//   - the buffered request is bigger than the run's inspection ceiling, so it
//     is refused rather than waved through unread (brokered:git:push-too-large);
//   - the inspector cannot answer from the request's own bytes — a thin pack
//     whose delta bases stayed on the forge, a body in a content-coding this
//     proxy cannot read past, a pack that is malformed or over one of
//     internal/gitpack's own ceilings (brokered:git:push-uninspectable);
//   - a path the push introduces matches a deny rule (brokered:git:push-rules).
//
// Otherwise the buffered bytes go onward unchanged.
//
// ENTERED INDEPENDENTLY OF BRANCH-NAMESPACE CONFINEMENT. Both brokered lanes
// call this step outside the block that decides whether confinePush runs, and
// that placement is the control: git_push_any_branch is an opt-out of WHERE a
// push may go, and a WHERE opt-out must never switch off a WHAT control. Wired
// inside that block instead, a policy carrying deny_paths and
// git_push_any_branch: true would read as governed and enforce nothing.
//
// REFUSED BEFORE THE CREDENTIAL IS MINTED. Both call sites run this ahead of
// their token mint, so a refused push never causes a credential to be issued —
// the same ordering confinePush already relies on.
//
// OFFENDING PATHS DO NOT RIDE THE DECISION LOG. They go to the structured log
// and, at most ten of them, to the refusal body. The decision log's free-text
// fields (egress.DecisionLog's Cause and Via) are reserved for dial-shaped
// refusals and stay empty here.
//
// git renders a receive-pack 403 as "error: RPC failed; HTTP 403" and drops
// the body, so the person reads the paths from the run's decision stream and
// this log rather than from their terminal. A sideband report-status would
// render, and is refused here for confinePush's reason: it would claim
// "unpack ok" for a pack that was never forwarded.
//
// WHAT THE RULES SEE is the pack and nothing else (internal/gitpack's package
// comment, docs/POLICIES.md and threatmodel/THREAT-MODEL.md carry this in
// full). Two consequences shape how a deny pattern behaves: a directory the
// push did not change is not in the pack and is skipped, so a pattern inside
// it does not fire — including for a directory resurrected wholesale out of
// the forge's own history; and, because a governed push always lands on the
// run's own branch and so has no pre-image to diff against, the new tree is
// enumerated and every file at the repository ROOT is reported whether the
// push touched it or not. Entries the pack cannot measure are matched rather
// than dropped: a blob the forge already stores, re-introduced at a denied
// path by a rename, is reported identically to an untouched root-level file,
// so dropping one would drop the other.
//
// Phase one has no size rule, so nothing here compares gitpack.Change.Size.
// Whoever adds max_file_size_mib must DECIDE what Size == -1 means rather than
// compare it: -1 is "the pack does not carry this blob", and it passes every
// "is it under the limit" test by accident.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// ruleSourceGitRules marks a brokered push refused because a path it
	// introduces matched push_rules.deny_paths.
	ruleSourceGitRules = "brokered:git:push-rules"
	// ruleSourceGitPackBig marks a brokered push refused because its body is
	// larger than push_rules.max_inspect_pack_mib. Refused rather than held:
	// holding would ask a person to approve a push nobody inspected.
	ruleSourceGitPackBig = "brokered:git:push-too-large"
	// ruleSourceGitPackBlind marks a brokered push refused because the
	// inspector could not answer from the request's own bytes.
	ruleSourceGitPackBlind = "brokered:git:push-uninspectable"
	// defaultInspectPackMiB is the ceiling a run that sets push_rules without
	// naming max_inspect_pack_mib gets. It sits BELOW the 0..64 range
	// validatePushRules admits on purpose: raising the ceiling is the stated
	// remedy for a too-large refusal, and a default at the maximum would leave
	// an operator nothing to raise.
	defaultInspectPackMiB = 32
	// maxDeniedPathsInBody caps the offending paths the sandbox is told about.
	maxDeniedPathsInBody = 10
	// maxDeniedPathsLogged caps the offending paths the structured log carries.
	// The COUNT is always exact; the list is a sample, because a first push to
	// a new branch is enumerated whole and can match six figures of paths.
	maxDeniedPathsLogged = 100
	// maxGlobOps bounds the pattern-against-segment comparisons one push may
	// cost. deny_paths carries no count cap (#265: a clamp's union can legally
	// be longer than either side authored), so the list is bounded only by the
	// 1 MiB control-plane body — around 131,000 entries — while a push may
	// carry gitpack's 200,000 changed paths. That product is reachable from
	// inside a sandbox, so the matcher bounds its own work and refuses over the
	// ceiling instead of grinding, the same shape internal/gitpack's own tree
	// walk uses.
	maxGlobOps = 8 << 20
)

// errGlobBudget is the too-much-work refusal, reported to the caller as the
// same uninspectable outcome a pack over one of gitpack's ceilings gets: in
// both cases the rules could not be evaluated, and an unevaluated rule must
// never read as a pass.
var errGlobBudget = errors.New("the deny_paths list is too large to evaluate against this push")

// pushRuleSet is a run's push_rules compiled once, at policy-compile time, for
// per-request matching: the patterns are pre-split on "/" so a push carrying
// six figures of paths does not re-split the policy for every one of them.
// Nil means the run has no content rules (types.PushRulesSpec.IsSet).
type pushRuleSet struct {
	// deny holds each deny_paths entry split into segments.
	deny [][]string
	// inspectMax is how many bytes of the request are buffered before the push
	// is refused as too large.
	inspectMax int64
}

// compilePushRules builds the compiled form, or nil when the spec carries no
// actual rule. A leading "/" is trimmed because gitpack reports paths relative
// to the repository root with no leading separator, and an operator who writes
// "/infra/**" means the same thing as "infra/**" rather than a pattern that
// can never match.
func compilePushRules(s *types.PushRulesSpec) *pushRuleSet {
	if !s.IsSet() {
		return nil
	}
	rs := &pushRuleSet{inspectMax: int64(defaultInspectPackMiB) << 20}
	if s.MaxInspectPackMiB > 0 {
		rs.inspectMax = int64(s.MaxInspectPackMiB) << 20
	}
	for _, pat := range s.DenyPaths {
		rs.deny = append(rs.deny, strings.Split(strings.TrimPrefix(pat, "/"), "/"))
	}
	return rs
}

// match reports the paths a deny rule claims: a capped SAMPLE for the log and
// the exact total. It stops at the first pattern that matches a path — the
// answer is per path, not per rule.
func (rs *pushRuleSet) match(changes []gitpack.Change) (sample []string, total int, err error) {
	budget := maxGlobOps
	for _, c := range changes {
		segs := strings.Split(c.Path, "/")
		for _, pat := range rs.deny {
			ok, err := matchSegments(pat, segs, &budget)
			if err != nil {
				return nil, 0, err
			}
			if ok {
				total++
				if len(sample) < maxDeniedPathsLogged {
					sample = append(sample, c.Path)
				}
				break
			}
		}
	}
	return sample, total, nil
}

// matchSegments matches a pre-split deny pattern against a pre-split path.
//
// "**" matches zero or more whole segments; inside one segment the wildcards
// are path.Match's, which never cross a separator. The walk is the ordinary
// backtracking one, so its worst case is the product of the two lengths, and
// every segment comparison is charged against budget.
func matchSegments(pat, name []string, budget *int) (bool, error) {
	star, retry := -1, 0
	i, j := 0, 0
	for j < len(name) {
		switch {
		case i < len(pat) && pat[i] == "**":
			star, retry = i, j
			i++
		default:
			ok, err := matchSegment(pat, i, name[j], budget)
			if err != nil {
				return false, err
			}
			if ok {
				i++
				j++
				continue
			}
			if star < 0 {
				return false, nil
			}
			// Let the "**" swallow one more segment and try again.
			retry++
			i, j = star+1, retry
		}
	}
	for i < len(pat) && pat[i] == "**" {
		i++
	}
	return i == len(pat), nil
}

// matchSegment compares one pattern segment against one path segment, charging
// the comparison against budget.
//
// path.Match is the matcher rather than a hand-rolled one, so "*", "?" and
// character classes mean here exactly what they mean everywhere else in Go. An
// unterminated "[" is not a Go pattern at all; rather than let the rule
// silently match nothing — a deny rule that quietly does nothing is the worst
// outcome available — the segment is compared literally, which is what someone
// who put a bracket in a filename meant.
func matchSegment(pat []string, i int, seg string, budget *int) (bool, error) {
	if i >= len(pat) {
		return false, nil
	}
	if *budget <= 0 {
		return false, errGlobBudget
	}
	*budget--
	ok, err := path.Match(pat[i], seg)
	if err != nil {
		return pat[i] == seg, nil
	}
	return ok, nil
}

// nonIdentityEncoding reports a Content-Encoding this proxy cannot read past,
// and the value to name in the refusal. git does not compress receive-pack
// bodies (remote-curl sets gzip_request for fetch only), but an encoded body
// must never be waved through unparsed: that is a silent bypass of every rule
// that reads the body.
func nonIdentityEncoding(h http.Header) (string, bool) {
	encs := h.Values("Content-Encoding")
	if len(encs) > 1 {
		return strings.Join(encs, ","), true
	}
	if len(encs) == 1 && encs[0] != "" && !strings.EqualFold(encs[0], "identity") {
		return encs[0], true
	}
	return "", false
}

// applyPushRules is the push CONTENT-rules step, shared by both brokered git
// lanes exactly as confinePush is: it buffers the request to the run's
// inspection ceiling, reads what the push would change, and refuses a push
// that is too large, unreadable, or carries a denied path.
//
// body is what the previous step left to forward — r.Body, or confinePush's
// command section followed by the still-streaming pack — and the reader
// returned on success replays those same bytes. On a refusal it writes the
// response itself, through deny so each lane records the decision against ITS
// OWN host, and returns ok=false. The refusal text lives here for confinePush's
// reason: two lanes only say the same thing forever if one place says it.
//
// A run whose policy carries no content rules is returned its body untouched
// and nothing is buffered, so a nil push_rules behaves exactly as it does
// today.
func (p *Proxy) applyPushRules(w http.ResponseWriter, r *http.Request, body io.Reader,
	subject slog.Attr, deny func(ruleSource string)) (io.Reader, bool) {
	rules := p.policy.contentRules()
	if rules == nil {
		return body, true
	}
	if enc, bad := nonIdentityEncoding(r.Header); bad {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on a "+enc+"-encoded push body")
		return nil, false
	}
	// One byte past the ceiling is how "over it" is known without reading what
	// is past it.
	buf, err := io.ReadAll(io.LimitReader(body, rules.inspectMax+1))
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules: the push body could not be read")
		return nil, false
	}
	if int64(len(buf)) > rules.inspectMax {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBig, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("wardyn: this push is larger than the %d MiB its content rules can inspect"+
				"\npush fewer commits, or ask an operator to raise push_rules.max_inspect_pack_mib",
				rules.inspectMax>>20))
		return nil, false
	}
	res, err := gitpack.Inspect(buf)
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on this push: "+err.Error()+
				"\npush from a complete clone (git fetch --unshallow) so the pack carries"+
				" every object it deltifies against")
		return nil, false
	}
	sample, total, err := rules.match(res.Changes)
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on this push: "+err.Error()+
				"\nask an operator to shorten push_rules.deny_paths")
		return nil, false
	}
	if total > 0 {
		deny(ruleSourceGitRules)
		slog.WarnContext(r.Context(), "wardyn-proxy: git push denied by content rules",
			slog.String("run_id", p.runID.String()),
			subject,
			slog.Int("denied_paths", total),
			slog.Any("paths", sample))
		http.Error(w, deniedPathsBody(sample, total), http.StatusForbidden)
		return nil, false
	}
	return bytes.NewReader(buf), true
}

// refusePush records, logs and answers one content-rule refusal that names no
// path — the too-large and uninspectable outcomes, whose whole cause is in the
// message. The denied-path refusal writes its own, because it carries a list.
func (p *Proxy) refusePush(w http.ResponseWriter, r *http.Request, subject slog.Attr,
	deny func(ruleSource string), ruleSource string, status int, msg string) {
	deny(ruleSource)
	slog.WarnContext(r.Context(), "wardyn-proxy: git push refused by content rules",
		slog.String("run_id", p.runID.String()),
		subject,
		slog.String("rule_source", ruleSource),
		slog.String("reason", msg))
	http.Error(w, msg, status)
}

// deniedPathsBody is the refusal git shows the person: the paths that matched,
// capped, and what to do about it.
func deniedPathsBody(sample []string, total int) string {
	var b strings.Builder
	b.WriteString("wardyn: this push is refused by the run's push content rules\n")
	shown := min(len(sample), maxDeniedPathsInBody)
	for _, pth := range sample[:shown] {
		b.WriteString("  " + pth + "\n")
	}
	if total > shown {
		fmt.Fprintf(&b, "  ... and %d more denied path(s)\n", total-shown)
	}
	b.WriteString("remove these paths from the push, or ask an operator to widen push_rules.deny_paths")
	return b.String()
}
