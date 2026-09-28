// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// Push CONTENT rules: what a brokered git push may contain, as opposed to
// where it may land (confinePush is the WHERE).
//
// Three outcomes:
//   - the buffered request or an inspectable pack exceeds an inspection/
//     gitpack ceiling, so it's refused rather than waved through unread
//     (brokered:git:push-too-large) — fix: push fewer commits at a time;
//   - the inspector can't answer from the request's own bytes (thin pack,
//     unreadable content-coding, malformed pack) (brokered:git:push-uninspectable);
//   - an introduced path matches a deny rule (brokered:git:push-rules).
//
// A push none of those refuses, but that introduces a path matching
// require_review_paths, is HELD for an admin's decision (push_hold.go) —
// matched the same way as a deny rule, and only once no deny rule matched.
//
// Entered independently of branch-namespace confinement: both brokered lanes
// call this step outside the block that decides whether confinePush runs.
// git_push_any_branch is a WHERE opt-out and must never switch off this WHAT
// control — wiring it inside that block would let a policy with deny_paths
// and git_push_any_branch: true read as governed while enforcing nothing.
//
// SECURITY: a refused push is never forwarded, even though its credential may
// already exist (both lanes run this ahead of their token lookup, so a push
// refused by its own bytes never mints one). Discovery (GET info/refs before
// every push) already minted or reused the credential, so the forge read
// these rules need normally reuses it too — these rules decide what reaches
// the forge, not whether a credential is issued.
//
// Offending paths never ride the decision log's free-text fields (reserved
// for dial-shaped refusals); they go to the structured log and, capped at
// ten, to the refusal body — git drops a receive-pack 403's body, so the
// person reads paths from the run's decision stream and this log, not their
// terminal. A sideband report-status would render but is refused for
// confinePush's reason: it would claim "unpack ok" for a pack never forwarded.
//
// What the rules see (full detail in internal/gitpack's package comment,
// docs/POLICIES.md, threatmodel/THREAT-MODEL.md): a pack omits every object
// the forge already stores, and a governed push lands on the run's own
// branch, so the new tree is enumerated fully — every root file is reported
// whether touched or not, and every directory the pack doesn't carry is one
// OPAQUE entry (symlinks and submodules are opaque the same way, since a
// checkout resolves paths beneath them to content no tree entry here names).
// An opaque entry matches a pattern that could match anything beneath it
// (matchesBeneath).
//
// History the pack re-sends is cleared against the forge first (push_forge.go).
// A matched entry the pack carries is refused from the pack alone; one it
// doesn't carry is compared against the forge's copy at the same path in a
// commit the push builds on — unchanged means dropped. Every other outcome
// (different/absent entry, no vouching commit, unreadable forge) refuses.
//
// Phase one has no size rule; a future max_file_size_mib would use
// gitpack.Change.Within/Size once "unknown" is resolved.

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
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// ruleSourceGitRules marks a brokered push refused because a path it
	// introduces matched push_rules.deny_paths.
	ruleSourceGitRules = "brokered:git:push-rules"
	// ruleSourceGitPackBig marks a push refused because its body exceeds
	// push_rules.max_inspect_pack_mib, or an inspectable pack costs more than
	// gitpack's own ceilings (gitpack.ErrTooLarge). Refused rather than held —
	// holding would ask someone to approve a push nobody inspected. Fix: push
	// fewer commits at a time.
	ruleSourceGitPackBig = "brokered:git:push-too-large"
	// ruleSourceGitPackBlind marks a push refused because the inspector
	// couldn't answer from the request's own bytes (thin pack, unreadable
	// shape, gitpack.ErrUninspectable) or a control error left the rules
	// unable to run.
	ruleSourceGitPackBlind = "brokered:git:push-uninspectable"
	// ruleSourceGitForgeRead marks the broker's own credentialed reads of
	// api.github.com on a push's behalf — one ALLOW row per push that read
	// the forge, so an egress review sees them.
	ruleSourceGitForgeRead = "brokered:git:forge-read"
	// defaultInspectPackMiB is the default ceiling for push_rules that don't
	// set max_inspect_pack_mib. Kept below the 0..64 range validatePushRules
	// admits, since raising the ceiling is the remedy for a too-large refusal.
	defaultInspectPackMiB = 32
	// maxDeniedPathsInBody caps the offending paths the sandbox is told about.
	maxDeniedPathsInBody = 10
	// maxDeniedPathsLogged caps the offending paths the structured log
	// carries. The count is always exact; the list is a sample (a first push
	// to a new branch can match six figures of paths).
	maxDeniedPathsLogged = 100
	// maxGlobOps bounds the pattern-against-segment comparisons one push may
	// cost. deny_paths has no count cap (a clamp's union can be longer than
	// either side authored), so up to ~131,000 patterns can meet 200,000
	// changed paths inside a sandbox — the matcher bounds its own work and
	// refuses over the ceiling rather than grinding, like gitpack's own tree
	// walk.
	maxGlobOps = 8 << 20
)

// errGlobBudget is the too-much-work refusal, reported like a pack over one
// of gitpack's ceilings: an unevaluated rule must never read as a pass.
var errGlobBudget = errors.New("the push_rules pattern list is too large to evaluate against this push")

// pushRuleSet is a run's push_rules compiled once, at policy-compile time, so
// per-request matching doesn't re-split the policy for every path a push
// carries. Nil means no content rules (types.PushRulesSpec.IsSet).
type pushRuleSet struct {
	// deny holds each deny_paths entry split into segments.
	deny [][]string
	// review holds each require_review_paths entry, split the same way.
	review [][]string
	// hold is how long a push a review rule matches waits for its decision.
	hold time.Duration
	// unreadable is the first deny_paths/require_review_paths entry
	// types.DenyPathSegments refused. Write-time validation should already
	// catch this; if a policy bypassed it, every push is refused rather than
	// silently compiling to a pattern that matches nothing.
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
	rs := &pushRuleSet{inspectMax: int64(defaultInspectPackMiB) << 20, hold: defaultPushHold}
	if s.MaxInspectPackMiB > 0 {
		rs.inspectMax = int64(s.MaxInspectPackMiB) << 20
	}
	if s.HoldSeconds > 0 {
		// The sidecar's last door, as configureHold is for first_use_hold_seconds:
		// a policy stored before its bound existed reaches here unvalidated.
		rs.hold = min(time.Duration(s.HoldSeconds)*pushHoldSecond, maxHoldTimeout)
	}
	rs.deny = rs.compile(s.DenyPaths)
	rs.review = rs.compile(s.RequireReviewPaths)
	return rs
}

// compile splits each entry, recording the first one it cannot read.
func (rs *pushRuleSet) compile(pats []string) [][]string {
	var out [][]string
	for _, pat := range pats {
		segs, err := types.DenyPathSegments(pat)
		if err != nil {
			rs.unreadable = cmp.Or(rs.unreadable, pat)
			continue
		}
		out = append(out, segs)
	}
	return out
}

// match reports the entries one of pats claims: those the pack carries come
// back in hits (named as shownPath names them), those it doesn't carry come
// back whole in unknown for the forge to clear or not (push_forge.go). Stops
// at the first pattern that claims an entry — the answer is per path.
//
// An opaque entry (gitpack.Change.Opaque: uncarried directory, symlink,
// submodule) is claimed when a pattern could match anything beneath it, since
// what's beneath is unknown and must be treated as matched.
//
// hits holds references to the Change paths, not copies, and is every
// claimed path (the held-push dedup key needs them all).
func match(pats [][]string, changes []gitpack.Change) (hits []string, unknown []gitpack.Change, err error) {
	budget := maxGlobOps
	for _, c := range changes {
		var segs []string // the root is zero segments, not one empty one
		if c.Path != "" {
			segs = strings.Split(c.Path, "/")
		}
		opaque := c.Opaque()
		for _, pat := range pats {
			ok, err := matchSegments(pat, segs, &budget)
			if err == nil && !ok && opaque {
				ok, err = matchesBeneath(pat, segs, &budget)
			}
			if err != nil {
				return nil, nil, err
			}
			if ok {
				if c.Carried() {
					hits = append(hits, shownPath(c))
				} else {
					unknown = append(unknown, c)
				}
				break
			}
		}
	}
	return hits, unknown, nil
}

// sampleOf is the capped slice of paths the structured log carries.
func sampleOf(paths []string) []string {
	return paths[:min(len(paths), maxDeniedPathsLogged)]
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
// name — whether the pattern can consume every segment of name and still
// have a segment left for what lies under it. at[i] is true when pat[:i]
// matches the segments read so far; "**" may match none of them and stays
// in place to consume another.
//
// A remaining segment is assumed to match — the conservative answer for a
// deny rule.
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
// "**" matches zero or more whole segments; wildcards inside one segment are
// path.Match's and never cross a separator. Ordinary backtracking walk —
// worst case the product of the two lengths — charged against budget.
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

// matchSegment compares one pattern segment against one path segment,
// charging the comparison against budget.
//
// path.Match is used so "*", "?" and character classes mean what they mean
// everywhere else in Go. An unterminated "[" isn't a valid Go pattern; rather
// than let the rule silently match nothing, the segment is compared
// literally, matching what someone who put a bracket in a filename meant.
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

// nonIdentityEncoding reports a Content-Encoding this proxy can't read past,
// and the value to name in the refusal. git doesn't compress receive-pack
// bodies, but an encoded body must never be waved through unparsed — that
// would silently bypass every rule that reads the body.
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
// lanes exactly as confinePush is: buffers the request to the run's
// inspection ceiling, reads what the push would change, refuses a push that
// is too large, unreadable, or carries a denied path, and holds one that
// needs review until an admin decides (push_hold.go).
//
// body is what the previous step left to forward; the returned reader
// replays the same bytes on success. On refusal it writes the response
// itself (through deny, so each lane records against its own host) and
// returns ok=false — text lives here so both lanes say the same thing.
//
// A run with no content rules gets its body back untouched, unbuffered.
//
// forge reads what the pack doesn't carry from the lane's own forge; nil
// when the lane can't (keeps the strict reading, push_forge.go). target is
// what a held push's approval names as repository and identity.
//
// The returned release MUST be deferred by the caller (like
// scanBufferedBody's): the buffer stays charged to scanRetained until the
// forwarded request is done with it. Always non-nil, safe to call more than
// once.
func (p *Proxy) applyPushRules(w http.ResponseWriter, r *http.Request, body io.Reader,
	subject slog.Attr, deny func(ruleSource string), forge *forgeRepo, target pushTarget) (io.Reader, func(), bool) {
	noRelease := func() {}
	rules := p.policy.contentRules()
	if rules == nil {
		return body, noRelease, true
	}
	buf, review, release, ok := p.inspectPush(w, r, rules, body, subject, deny, forge)
	if !ok {
		return nil, noRelease, false
	}
	// Held outside the inspection slot (already released by inspectPush): a
	// hold lasts minutes and the slot belongs to the whole sidecar.
	if len(review.paths) > 0 && !p.holdPush(w, r, rules, review, target, subject, deny) {
		release()
		return nil, noRelease, false
	}
	return bytes.NewReader(buf), release, true
}

// inspectPush is applyPushRules' work inside the inspection slot: the buffer,
// the verdict, and — when no deny rule matched — what the review rules match.
func (p *Proxy) inspectPush(w http.ResponseWriter, r *http.Request, rules *pushRuleSet, body io.Reader,
	subject slog.Attr, deny func(ruleSource string), forge *forgeRepo) ([]byte, pushReview, func(), bool) {
	noRelease := func() {}
	if rules.unreadable != "" {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			fmt.Sprintf("wardyn: cannot enforce push content rules: push_rules entry %q is not a"+
				" pattern the broker can read\nask an operator to correct it", rules.unreadable))
		return nil, pushReview{}, noRelease, false
	}
	if enc, bad := nonIdentityEncoding(r.Header); bad {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on a "+enc+"-encoded push body")
		return nil, pushReview{}, noRelease, false
	}
	// Shares the LLM inspection path's slot and byte budget (scanBufferedBody):
	// the sidecar runs under a hard 256 MiB cgroup cap, and a push is a small
	// body that can inflate to what gitpack's ceilings allow (four compressed
	// 31 MiB blobs is a 34 KB request and 124 MiB of heap; three at once
	// OOM-kill the run's only network path). A wait that expires fails closed
	// — the push is refused, never forwarded unread.
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
		return nil, pushReview{}, noRelease, false
	}
	// One byte past the ceiling is how "over it" is known without reading what
	// is past it.
	buf, err := io.ReadAll(io.LimitReader(body, rules.inspectMax+1))
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules: the push body could not be read")
		return nil, pushReview{}, noRelease, false
	}
	if int64(len(buf)) > rules.inspectMax {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBig, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("wardyn: this push is larger than the %d MiB its content rules can inspect"+
				"\npush fewer commits, or ask an operator to raise push_rules.max_inspect_pack_mib",
				rules.inspectMax>>20))
		return nil, pushReview{}, noRelease, false
	}
	res, err := gitpack.Inspect(buf)
	if errors.Is(err, gitpack.ErrTooLarge) {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBig, http.StatusRequestEntityTooLarge,
			"wardyn: cannot enforce push content rules on this push: "+err.Error()+
				"\npush fewer commits at a time")
		return nil, pushReview{}, noRelease, false
	}
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on this push: "+err.Error()+
				"\npush from a complete clone (git fetch --unshallow) so the pack carries"+
				" every object it deltifies against")
		return nil, pushReview{}, noRelease, false
	}
	denied, why, err := p.matchedPaths(r, rules.deny, res, forge, subject)
	if err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on this push: "+err.Error()+
				"\nask an operator to shorten push_rules.deny_paths")
		return nil, pushReview{}, noRelease, false
	}
	if len(denied) > 0 {
		deny(ruleSourceGitRules)
		slog.WarnContext(r.Context(), "wardyn-proxy: git push denied by content rules",
			slog.String("run_id", p.runID.String()),
			subject,
			slog.Int("denied_paths", len(denied)),
			slog.Any("paths", sampleOf(denied)),
			slog.String("reason", why))
		http.Error(w, deniedPathsBody(denied, why), http.StatusForbidden)
		return nil, pushReview{}, noRelease, false
	}
	review := pushReview{cmds: res.Commands}
	if review.paths, review.why, err = p.matchedPaths(r, rules.review, res, forge, subject); err != nil {
		p.refusePush(w, r, subject, deny, ruleSourceGitPackBlind, http.StatusUnsupportedMediaType,
			"wardyn: cannot enforce push content rules on this push: "+err.Error()+
				"\nask an operator to shorten push_rules.require_review_paths")
		return nil, pushReview{}, noRelease, false
	}
	// Buffer outlives the slot, forwarded or held from here; charged while
	// the slot is still held so scanRetained keeps one acquirer.
	release, ok := retainScanBuffer(ctx, len(buf))
	if !ok {
		busy()
		return nil, pushReview{}, noRelease, false
	}
	return buf, review, release, true
}

// matchedPaths is the verdict of one pattern list on one inspected push:
// every path it claims, and why when the forge was asked (a push the pack
// alone passes is never asked). History the pack re-sends is cleared first
// (forgeRepo.settle); an entry the pack still carries is claimed outright,
// and only then does the forge get to clear entries the pack doesn't carry.
func (p *Proxy) matchedPaths(r *http.Request, pats [][]string, res gitpack.Result, forge *forgeRepo,
	subject slog.Attr) (paths []string, why string, err error) {
	if len(pats) == 0 {
		return nil, "", nil
	}
	paths, unknown, err := match(pats, res.Changes)
	if err != nil || len(paths) == 0 && len(unknown) == 0 {
		return paths, "", err
	}
	ctx, cancel := context.WithTimeout(r.Context(), forgeReadWait)
	defer cancel()
	left := unknown
	if res, why = forge.settle(ctx, res); why == "" {
		if paths, unknown, err = match(pats, res.Changes); err != nil {
			return nil, "", err
		}
		left = nil
		if len(paths) == 0 && len(unknown) > 0 {
			left, why = forge.unchanged(ctx, unknown, res)
		}
	}
	for _, c := range left {
		paths = append(paths, shownPath(c))
	}
	if forge != nil && forge.reads > 0 {
		p.sink.emit(decisionLog(egress.Request{RunID: p.runID, Host: githubAPIHost, Port: 443,
			Method: http.MethodGet, Time: p.now()}, egress.Allow, ruleSourceGitForgeRead))
		slog.InfoContext(r.Context(), "wardyn-proxy: git push content rules read the forge",
			slog.String("run_id", p.runID.String()),
			subject,
			slog.Int("still_matched", len(paths)),
			slog.Int("forge_reads", forge.reads))
	}
	return paths, why, nil
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
func deniedPathsBody(paths []string, why string) string {
	return pathsBody("wardyn: this push is refused by the run's push content rules", paths, "denied", why,
		"remove these paths from the push, or ask an operator to widen push_rules.deny_paths")
}

// pathsBody is the shape every path-naming push refusal shares: a headline, at
// most maxDeniedPathsInBody of paths and the count of the rest, why the forge
// did not clear them when asked, and a remedy.
func pathsBody(headline string, paths []string, noun, why, remedy string) string {
	var b strings.Builder
	b.WriteString(headline + "\n")
	shown := min(len(paths), maxDeniedPathsInBody)
	for _, pth := range paths[:shown] {
		b.WriteString("  " + pth + "\n")
	}
	if len(paths) > shown {
		fmt.Fprintf(&b, "  ... and %d more %s path(s)\n", len(paths)-shown, noun)
	}
	if slices.ContainsFunc(sampleOf(paths), func(s string) bool { return strings.HasSuffix(s, "/") }) {
		b.WriteString("a path ending in / is a directory this push does not carry (/ alone is the whole tree)\n")
	}
	if why != "" {
		b.WriteString(why + "\n")
	}
	b.WriteString(remedy)
	return b.String()
}
