// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// What a branch a push updates ends up holding, against its own tip.
//
// unchanged (push_forge.go) clears what the pack does not carry against any
// commit the push builds on that the forge vouches for. That is the question
// for what each commit introduces, and the whole question for a branch the push
// creates. For a branch it updates it leaves a gap: a commit the push builds on
// can be OLDER than the branch's tip. A merge naming an older commit of the
// branch as a parent, or a second ref the same push creates on one, vouches for
// that commit's version of a path, so the branch would hold it again with no
// refusal and no hold.
//
// So for deny_paths and require_review_paths every command that moves an
// existing branch is also read the other way round: each entry its new tree
// holds at a path a pattern claims must be what the branch's tip, as the forge
// confirms it (updatedTip), holds at that path. A directory the pack does not
// carry, where the tip holds another directory, is looked into, both listed
// from the forge, as far as a pattern could claim. Anything else is a change on
// that branch and is refused or held like a carried one, wherever it came
// from: merging in a commit that changed such a path, from the default branch
// or anywhere else, is held too. A ref the push creates has no tip to compare
// with, and a removal is no match, as everywhere in these rules. A tag or any
// other ref outside refs/heads/ is judged as before: deleting it and creating
// it again on any commit would pass anyway. A push that carries no object
// moves a branch to a commit the forge already holds (forgeRepo.held), so that
// commit's whole tree is the entry, read from the forge. Deleting a branch is
// refused or held: one created again under its name would have no tip.
//
// The tip's trees are the ones unchanged reads already, so this is mostly
// cache hits. Its reads count against the same maxForgeReads, each call has its
// own forgeReadWait, and every failure keeps the strict reading.

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/gitpack"
)

// The reasons a refusal gives for entries the tip comparison found or could not check.
const (
	whyTipChanged    = "a directory or file above is not what the same path holds at the tip of a branch this push updates"
	whyNoTip         = "this push moves a branch the forge does not show at the tip the push states, so nothing shows what that branch held at these paths"
	whyTipUnread     = "the broker could not compare what this push leaves on a branch it updates with that branch's tip: "
	whyBranchDeleted = "this push deletes a branch, and a branch created again under that name is not compared with what this one held"
)

// packTips is gitpack.Result.Tips. A var so a test can make a walk run past its bounds.
var packTips = gitpack.Result.Tips

// tipRemedy is what the person can do about one of the reasons above, and ""
// for any other reason. Taking a path out of the push changes nothing here:
// the comparison is of what the branch would hold, not of what the push adds.
func tipRemedy(why string) string {
	switch {
	case why == whyTipChanged:
		return "keep what the branch's tip holds at these paths, or push to a new branch"
	case why == whyNoTip:
		return "fetch the branch and push again, or push to a new branch"
	case why == whyBranchDeleted:
		return "leave the branch in place, or delete it on the forge"
	case strings.HasPrefix(why, whyTipUnread):
		return "try the push again; if it keeps failing, ask an operator"
	}
	return ""
}

// tipChanges runs forgeRepo.beyondTips for one pattern list inside its own
// forgeReadWait, and records a forge read as claimedEntries does.
func (p *Proxy) tipChanges(r *http.Request, pats [][]string, res gitpack.Result, forge *forgeRepo) ([]gitpack.Change, string, error) {
	if forge == nil {
		return nil, "", nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), forgeReadWait)
	defer cancel()
	reads := forge.reads
	left, why, err := forge.beyondTips(ctx, pats, res)
	if forge.reads > reads {
		p.sink.emit(decisionLog(egress.Request{RunID: p.runID, Host: forge.apiHost(), Port: 443,
			Method: http.MethodGet, Time: p.now(), Relayed: p.relayLane(r.Context())}, egress.Allow, ruleSourceGitForgeRead))
	}
	return left, why, err
}

// beyondTips returns every entry pats claim that a command moving an existing
// branch leaves where the branch's tip holds something else, and why: every
// such command is read, whatever an earlier one came to. A walk past gitpack's
// ceilings reads as the whole tree ("/") left.
func (f *forgeRepo) beyondTips(ctx context.Context, pats [][]string, res gitpack.Result) ([]gitpack.Change, string, error) {
	budget := maxGlobOps
	tips, err := packTips(res, func(c gitpack.Change) (bool, error) { return claims(pats, c, &budget) })
	if errors.Is(err, errGlobBudget) {
		return nil, "", err
	}
	if err != nil {
		return []gitpack.Change{{Mode: gitpack.ModeUncarried}}, whyTipUnread + err.Error(), nil
	}
	// A branch that cannot be compared keeps its entries whole and the rest are
	// still compared, so a hold names every one. The reason is the first that
	// leaves the push unresolved, then a deleted branch, then a changed entry.
	var left []gitpack.Change
	why := whyTipChanged
	because := func(w string) {
		if why == whyTipChanged || why == whyBranchDeleted && w != whyTipChanged {
			why = w
		}
	}
	for i, c := range res.Commands {
		if strings.Trim(c.Old, "0") == "" || c.New == c.Old || !strings.HasPrefix(c.Ref, "refs/heads/") {
			continue
		}
		if strings.Trim(c.New, "0") == "" {
			// A branch created again has no tip, so deleting one first would undo the comparison.
			left = append(left, gitpack.Change{Mode: gitpack.ModeUncarried})
			because(whyBranchDeleted)
			continue
		}
		entries := tips[i]
		if res.Empty() {
			// The forge already holds the new commit, so its whole tree is the entry, read there.
			entries = []gitpack.Change{{Mode: gitpack.ModeUncarried}}
		}
		if len(entries) == 0 {
			continue
		}
		root, err := f.tipRoot(ctx, c, res.Commands)
		if err == nil && root != "" && res.Empty() {
			entries[0].OID, err = f.vouch(ctx, c.New, res.Commands)
		}
		switch {
		case err != nil:
			left = append(left, entries...)
			because(f.unread(whyTipUnread, err))
			continue
		case root == "":
			left = append(left, entries...)
			because(whyNoTip)
			continue
		}
		for j, e := range entries {
			more, err := f.beyondTip(ctx, root, e, pats, &budget)
			if errors.Is(err, errGlobBudget) {
				return nil, "", err
			}
			if err != nil {
				left = append(left, entries[j:]...)
				because(f.unread(whyTipUnread, err))
				break
			}
			left = append(left, more...)
		}
	}
	return left, why, nil
}

// tipRoot is the root tree of the tip the forge confirms for the branch c
// moves, and "" when it confirms none.
func (f *forgeRepo) tipRoot(ctx context.Context, c gitpack.Command, cmds []gitpack.Command) (string, error) {
	tip, err := f.updatedTip(ctx, c)
	if err != nil || tip == "" {
		return "", err
	}
	return f.vouch(ctx, tip, cmds)
}

// beyondTip returns what of e the tree root does not hold at e's path: nothing
// when root holds e, and e when root lacks it or holds something else there.
// For a directory the pack does not carry, where root holds another directory,
// it is what the same question finds for each entry beneath it that pats could
// claim, both directories listed from the forge.
func (f *forgeRepo) beyondTip(ctx context.Context, root string, e gitpack.Change, pats [][]string, budget *int) ([]gitpack.Change, error) {
	// Listings are cached, so a walk that reads nothing new still ends with forgeReadWait.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	held, err := f.heldAtAny(ctx, []string{root}, e)
	if err != nil || held {
		return nil, err
	}
	if e.Mode != gitpack.ModeUncarried {
		return []gitpack.Change{e}, nil
	}
	if was, ok, err := f.entryAt(ctx, root, e.Path); err != nil || !ok || was.Type != "tree" {
		return []gitpack.Change{e}, err
	}
	now, err := f.listing(ctx, e.OID)
	if err != nil {
		return nil, err
	}
	var out []gitpack.Change
	for _, name := range slices.Sorted(maps.Keys(now)) {
		n := now[name]
		child := gitpack.Change{Path: path.Join(e.Path, name), Mode: n.Mode, OID: n.SHA}
		if n.Type == "tree" {
			child.Mode = gitpack.ModeUncarried
		}
		ok, err := claims(pats, child, budget)
		if err == nil && ok {
			var more []gitpack.Change
			more, err = f.beyondTip(ctx, root, child, pats, budget)
			out = append(out, more...)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
