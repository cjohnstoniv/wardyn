// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runnerRef is a runner an argument names: one of your self-hosted runners (self) or a remote-hosted
// runner (remote, its id).
type runnerRef struct {
	self   *uuid.UUID
	remote string
	name   string
	// unclaimed is set when the runner was read among your own and its owner has not claimed it yet.
	unclaimed bool
	// byID is set when the argument was a runner id, which is taken as given without a read.
	byID bool
}

// readClaims reads your own runners once when a runner was named by id, so one not claimed yet is known
// as that however it was named. A credential with no runners of its own is refused the list; the server
// then says what it thinks of the id.
func readClaims(cmd *cobra.Command, c *sdk.Client, refs []runnerRef) error {
	if !slices.ContainsFunc(refs, func(r runnerRef) bool { return r.byID }) {
		return nil
	}
	own, err := c.ListMyRunners(cmd.Context())
	if err := unlessRefused(err); err != nil {
		return err
	}
	for i, ref := range refs {
		for _, r := range own {
			if ref.byID && r.ID == *ref.self {
				refs[i].unclaimed = r.State == types.RunnerUnclaimed
			}
		}
	}
	return nil
}

// in reports whether the pool's members include this runner.
func (r runnerRef) in(members []sdk.RunnerPoolMemberView) bool {
	for _, m := range members {
		if (r.self != nil && m.ID == r.self.String()) || (r.self == nil && m.ID == r.remote) {
			return true
		}
	}
	return false
}

func (r runnerRef) kind() string {
	if r.self != nil {
		return "self-hosted"
	}
	return "remote-hosted"
}

// resolveRunner reads <runner>: a runner id names your self-hosted runner as given; otherwise the name
// (or a remote-hosted runner's id) is looked up among your own runners and the remote-hosted runners
// remotes lists. A name both kinds answer to is refused; one neither answers to is passed on as a
// remote-hosted runner's id, and the server's sentence says why it cannot join.
func resolveRunner(cmd *cobra.Command, c *sdk.Client, arg string, remotes func() ([]sdk.RemoteRunnerView, error)) (runnerRef, error) {
	if id, err := uuid.Parse(arg); err == nil {
		return runnerRef{self: &id, name: arg, byID: true}, nil
	}
	var found []runnerRef
	own, err := c.ListMyRunners(cmd.Context())
	if err := unlessRefused(err); err != nil {
		return runnerRef{}, err
	}
	for _, r := range own {
		if r.Name == arg {
			id := r.ID
			found = append(found, runnerRef{self: &id, name: arg, unclaimed: r.State == types.RunnerUnclaimed})
		}
	}
	rs, err := remotes()
	if err := unlessRefused(err); err != nil {
		return runnerRef{}, err
	}
	for _, r := range rs {
		if r.ID == arg || r.Name == arg {
			found = append(found, runnerRef{remote: r.ID, name: arg})
		}
	}
	switch len(found) {
	case 0:
		return runnerRef{remote: arg, name: arg}, nil
	case 1:
		return found[0], nil
	}
	return runnerRef{}, fmt.Errorf("%d runners answer to %q; name it by id", len(found), arg)
}

// resolveMember reads <runner> for a removal: a runner the pool holds, by its id or name. One the pool
// does not hold is refused, so a removal never reports a runner gone that was never matched.
func resolveMember(pool sdk.RunnerPool, members []sdk.RunnerPoolMemberView, arg string) (runnerRef, error) {
	var found []runnerRef
	for _, m := range members {
		if m.ID != arg && m.Name != arg {
			continue
		}
		ref := runnerRef{remote: m.ID, name: arg}
		if m.Kind == sdk.RunnerKindSelfHosted {
			id, err := uuid.Parse(m.ID)
			if err != nil {
				return runnerRef{}, fmt.Errorf("runner %q in %s has no runner id", arg, pool.Name)
			}
			ref = runnerRef{self: &id, name: arg}
		}
		found = append(found, ref)
	}
	switch len(found) {
	case 0:
		return runnerRef{}, fmt.Errorf("no runner in %s answers to %q: name one of its runners by id or name", pool.Name, arg)
	case 1:
		return found[0], nil
	}
	return runnerRef{}, fmt.Errorf("%d runners in %s answer to %q; name it by id", len(found), pool.Name, arg)
}

// unlessRefused drops a server's 4xx refusal of a list the caller may not read (an admin credential has no
// runners of its own; a person may not list every remote-hosted runner) and keeps any other error.
func unlessRefused(err error) error {
	var api *sdk.APIError
	if errors.As(err, &api) && api.Status < 500 {
		return nil
	}
	return err
}

// heldForApproval reports a write the server held for a second person as done for now (exit 0), in the
// words `wardyn governance` uses; any other error is returned as it is.
func heldForApproval(cmd *cobra.Command, err error) (bool, error) {
	var pending *sdk.PendingApprovalError
	if !errors.As(err, &pending) {
		return false, err
	}
	_, werr := fmt.Fprintf(cmd.OutOrStdout(), "pending approval: %d change(s) stored, not applied; a second approver runs `wardyn governance changes approve <id>`\n", len(pending.Changes))
	return true, werr
}

// joinAnd is "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	if n := len(items); n > 1 {
		return strings.Join(items[:n-1], ", ") + " and " + items[n-1]
	}
	return strings.Join(items, "")
}
