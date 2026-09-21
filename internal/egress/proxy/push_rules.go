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
// A REFUSED PUSH IS NEVER FORWARDED; ITS CREDENTIAL MAY ALREADY EXIST. Both
// call sites run this ahead of their token lookup, so a push passed by its own
// bytes mints nothing itself. One its pack alone would refuse asks the lane for
// the credential first, to read the forge (push_forge.go). Neither is a push's
// first request: git sends GET
// info/refs?service=git-receive-pack before every push, the forge will not
// advertise refs to it without the credential, and that discovery mints (or
// reuses) it — so the forge read normally reuses it too. These rules decide
// what reaches the forge, not whether a credential is issued: an
// approval-gated single-use grant is spent at discovery, and its cached
// credential serves the corrected retry.
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
// WHAT THE RULES SEE (internal/gitpack's package comment, docs/POLICIES.md
// and threatmodel/THREAT-MODEL.md carry this in full). A pack leaves out every
// object the forge already stores, wherever the new tree puts it, and a
// governed push lands on the run's own branch, so its parent stays on the
// forge and the new tree is enumerated: every file at the repository ROOT is
// reported whether the push touched it or not, and every directory the pack
// does not carry is reported as one OPAQUE entry — nothing in the pack
// distinguishes a directory left alone from one moved, copied or restored onto
// that path. Symlinks and submodules are opaque the same way: a checkout
// resolves paths beneath them to content no tree entry here names. An opaque
// entry is matched when a pattern could match anything beneath it
// (matchesBeneath).
//
// Before any entry refuses the push, the history the pack re-sends — commits
// the forge already holds in the history it vouches for — is taken out of the
// answer (push_forge.go). A matched entry the rest of the pack CARRIES is then
// refused from the pack alone. One it does not carry is compared with the same
// path in a commit the push builds on, read from the forge: the same mode and
// object id there means the push left it unchanged, and it is dropped. Every
// other outcome — a different or absent entry, no commit the repository's own
// history vouches for, a forge that cannot be read — refuses, and says which.
//
// Phase one has no size rule, so nothing here compares gitpack.Change.Size.
// Whoever adds max_file_size_mib must DECIDE what Size == -1 means rather than
// compare it: -1 is "the pack does not carry this blob", and it passes every
// "is it under the limit" test by accident.

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"

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
	// unreadable is the first deny_paths entry types.DenyPathSegments refused.
	// Write-time validation refuses the same entries, so this is reached only
	// by a policy that bypassed it — and then every push is refused, because
	// compiling the entry to a pattern that matches nothing is the silent
	// non-enforcement the check exists to prevent.
	unreadable string
	// inspectMax is how many bytes of the request are buffered before the push
	// is refused as too large.
	inspectMax int64
}

// compilePushRules builds the compiled form, or nil when the spec carries no
// actual rule. Each entry is read through types.DenyPathSegments, the reading
// write-time validation uses too.
func compilePushRules(s *types.PushRulesSpec) *pushRuleSet {
	if !s.IsSet() {
		return nil
	}
	rs := &pushRuleSet{inspectMax: int64(defaultInspectPackMiB) << 20}
	if s.MaxInspectPackMiB > 0 {
		rs.inspectMax = int64(s.MaxInspectPackMiB) << 20
	}
	for _, pat := range s.DenyPaths {
		segs, err := types.DenyPathSegments(pat)
		if err != nil {
			rs.unreadable = cmp.Or(rs.unreadable, pat)
			continue
		}
		rs.deny = append(rs.deny, segs)
	}
	return rs
}

// match reports the entries a deny rule claims. Those the pack carries are
// refused outright: a capped SAMPLE for the log and the exact total. Those it
// does not carry come back whole in unknown, for the forge to clear or not
// (push_forge.go). It stops at the first pattern that claims an entry — the
// answer is per path, not per rule.
//
// An opaque entry (gitpack.Change.Opaque: a directory the pack does not carry,
// a symlink, a submodule) is claimed when a pattern could match anything
// beneath it, not only the entry itself: what is beneath it is unknown, and a
// deny rule has to treat unknown as matched.
func (rs *pushRuleSet) match(changes []gitpack.Change) (sample []string, total int, unknown []gitpack.Change, err error) {
	budget := maxGlobOps
	for _, c := range changes {
		var segs []string // the root is zero segments, not one empty one
		if c.Path != "" {
			segs = strings.Split(c.Path, "/")
		}
		opaque := c.Opaque()
		for _, pat := range rs.deny {
			ok, err := matchSegments(pat, segs, &budget)
			if err == nil && !ok && opaque {
				ok, err = matchesBeneath(pat, segs, &budget)
			}
			if err != nil {
				return nil, 0, nil, err
			}
			if ok {
				if c.Carried() {
					sample, total = claim(sample, total, c)
				} else {
					unknown = append(unknown, c)
				}
				break
			}
		}
	}
	return sample, total, unknown, nil
}

// claim adds c to a refusal's capped sample and exact count.
func claim(sample []string, total int, c gitpack.Change) ([]string, int) {
	if len(sample) < maxDeniedPathsLogged {
		sample = append(sample, shownPath(c))
	}
	return sample, total + 1
}

// shownPath is how a claimed entry is named to the person: an uncarried
// directory ends in "/" (the whole tree is "/"), so a refusal naming ".github/"
// reads as the directory it is rather than as a file.
func shownPath(c gitpack.Change) string {
	if c.Mode == gitpack.ModeUncarried {
		return c.Path + "/"
	}
	return c.Path
}

// matchesBeneath reports whether pat could match some path strictly beneath
// name: whether the pattern can consume every segment of name and still have a
// segment left for what lies under it. at[i] is true when pat[:i] can match
// the segments read so far; "**" may stand for none of them, and stays in
// place to consume another.
//
// A remaining segment is assumed to match some name — the conservative answer
// for a deny rule, and the true one for every pattern an operator would write.
func matchesBeneath(pat, name []string, budget *int) (bool, error) {
	at := make([]bool, len(pat)+1)
	at[0] = true
	spreadStars(pat, at)
	for _, seg := range name {
		next := make([]bool, len(pat)+1)
		for i := range pat {
			if !at[i] {
				continue
			}
			if pat[i] == "**" {
				next[i] = true
				continue
			}
			ok, err := matchSegment(pat, i, seg, budget)
			if err != nil {
				return false, err
			}
			next[i+1] = next[i+1] || ok
		}
		at = next
		spreadStars(pat, at)
	}
	return slices.Contains(at[:len(pat)], true), nil
}

// spreadStars lets each reachable "**" match zero segments.
func spreadStars(pat []string, at []bool) {
	for i, p := range pat {
		if at[i] && p == "**" {
			at[i+1] = true
		}
	}
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
//
// forge reads what the pack does not carry from the lane's own forge; nil
// when the lane cannot, which keeps the strict reading (push_forge.go).
//
// The returned release MUST be deferred by the caller, as scanBufferedBody's
// is: the buffer stays charged to scanRetained until the forwarded request is
// done with it. It is always non-nil and safe to call more than once.
func (p *Proxy) applyPushRules(w http.ResponseWriter, r *http.Request, body io.Reader,
	subject slog.Attr, deny func(ruleSource string), forge *forgeRepo) (io.Reader, func(), bool) {
	noRelease := func() {}
	rules := p.policy.contentRules()
	if rules == nil {
		return body, noRelease, true
	}
	if rules.unreadable != "" {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			fmt.Sprintf("wardyn: cannot enforce push content rules: push_rules.deny_paths entry %q is not a"+
				" pattern the broker can read\nask an operator to correct it", rules.unreadable))
		return nil, noRelease, false
	}
	if enc, bad := nonIdentityEncoding(r.Header); bad {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on a "+enc+"-encoded push body")
		return nil, noRelease, false
	}
	// The same slot and byte budget the LLM inspection path takes
	// (scanBufferedBody), for the same reason: the sidecar runs under a hard
	// 256 MiB cgroup cap, and a push is a small body the agent chooses that
	// inflates to what gitpack's ceilings allow — four compressed 31 MiB blobs
	// are a 34 KB request and 124 MiB of heap, and three at once OOM-kill the
	// run's only network path. Sharing scanSlots rather than keeping a slot of
	// its own is the point: the cap belongs to the process, so a push inflating
	// beside an LLM extraction is the same overrun as two of either. A wait
	// that expires fails CLOSED: the push is refused, never forwarded unread.
	ctx, cancel := context.WithTimeout(r.Context(), scanQueueWait)
	defer cancel()
	busy := func() {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusServiceUnavailable,
			"wardyn: cannot enforce push content rules: timed out waiting to inspect this push\nretry it")
	}
	select {
	case scanSlots <- struct{}{}:
		defer func() { <-scanSlots }()
	case <-ctx.Done():
		busy()
		return nil, noRelease, false
	}
	// One byte past the ceiling is how "over it" is known without reading what
	// is past it.
	buf, err := io.ReadAll(io.LimitReader(body, rules.inspectMax+1))
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules: the push body could not be read")
		return nil, noRelease, false
	}
	if int64(len(buf)) > rules.inspectMax {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBig, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("wardyn: this push is larger than the %d MiB its content rules can inspect"+
				"\npush fewer commits, or ask an operator to raise push_rules.max_inspect_pack_mib",
				rules.inspectMax>>20))
		return nil, noRelease, false
	}
	res, err := gitpack.Inspect(buf)
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on this push: "+err.Error()+
				"\npush from a complete clone (git fetch --unshallow) so the pack carries"+
				" every object it deltifies against")
		return nil, noRelease, false
	}
	sample, total, why, err := p.deniedPaths(r, rules, res, forge, subject)
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on this push: "+err.Error()+
				"\nask an operator to shorten push_rules.deny_paths")
		return nil, noRelease, false
	}
	if total > 0 {
		deny(ruleSourceGitRules)
		slog.WarnContext(r.Context(), "wardyn-proxy: git push denied by content rules",
			slog.String("run_id", p.runID.String()),
			subject,
			slog.Int("denied_paths", total),
			slog.Any("paths", sample),
			slog.String("reason", why))
		http.Error(w, deniedPathsBody(sample, total, why), http.StatusForbidden)
		return nil, noRelease, false
	}
	// The buffer outlives the slot: it is forwarded from here. Charged while the
	// slot is still held, so scanRetained keeps its single acquirer.
	if !scanRetained.acquire(ctx, len(buf)) {
		busy()
		return nil, noRelease, false
	}
	var once sync.Once
	n := len(buf)
	return bytes.NewReader(buf), func() { once.Do(func() { scanRetained.release(n) }) }, true
}

// deniedPaths is the verdict on one inspected push: what the rules refuse, and
// why when the forge was asked. A push the pack alone passes is never asked
// about. Otherwise, inside forgeReadWait, the history the pack re-sends is
// taken out first (forgeRepo.settle); an entry the rest of the pack carries is
// then refused outright, and only when none is does the forge get to clear
// the entries the pack does not carry.
func (p *Proxy) deniedPaths(r *http.Request, rules *pushRuleSet, res gitpack.Result, forge *forgeRepo,
	subject slog.Attr) (sample []string, total int, why string, err error) {
	sample, total, unknown, err := rules.match(res.Changes)
	if err != nil || total == 0 && len(unknown) == 0 {
		return sample, total, "", err
	}
	ctx, cancel := context.WithTimeout(r.Context(), forgeReadWait)
	defer cancel()
	left := unknown
	if res, why = forge.settle(ctx, res); why == "" {
		if sample, total, unknown, err = rules.match(res.Changes); err != nil {
			return nil, 0, "", err
		}
		left = nil
		if total == 0 && len(unknown) > 0 {
			left, why = forge.unchanged(ctx, unknown, res)
		}
	}
	for _, c := range left {
		sample, total = claim(sample, total, c)
	}
	if forge != nil && forge.reads > 0 {
		slog.InfoContext(r.Context(), "wardyn-proxy: git push content rules read the forge",
			slog.String("run_id", p.runID.String()),
			subject,
			slog.Int("still_refused", total),
			slog.Int("forge_reads", forge.reads))
	}
	return sample, total, why, nil
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
// capped, why the forge did not clear them when it was asked, and what to do.
func deniedPathsBody(sample []string, total int, why string) string {
	var b strings.Builder
	b.WriteString("wardyn: this push is refused by the run's push content rules\n")
	shown := min(len(sample), maxDeniedPathsInBody)
	for _, pth := range sample[:shown] {
		b.WriteString("  " + pth + "\n")
	}
	if total > shown {
		fmt.Fprintf(&b, "  ... and %d more denied path(s)\n", total-shown)
	}
	if slices.ContainsFunc(sample, func(s string) bool { return strings.HasSuffix(s, "/") }) {
		b.WriteString("a path ending in / is a directory this push does not carry (/ alone is the whole tree)\n")
	}
	if why != "" {
		b.WriteString(why + "\n")
	}
	b.WriteString("remove these paths from the push, or ask an operator to widen push_rules.deny_paths")
	return b.String()
}
