// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package erasure erases one person's retained records, scope by scope, and
// reports complete only when every scope asked for is. It owns the order, the
// idempotence and the partial-failure answer; each scope's work is a Step the
// caller supplies, because the steps need the API server's locks and stores.
//
// The fences that make an erasure hold across replicas are not built here: a
// destroyed subject key is revoked by the key store's durable destroyed_at
// check, and an erased run output by out-o2's tombstone. A Step only calls them.
package erasure

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Scope names one kind of record a person's erasure reaches.
type Scope string

const (
	// Credentials: the person's stored credentials and the key they sit under.
	Credentials Scope = "credentials"
	// AuditPersonalFields: the key every sealed audit field of the person is
	// under, destroyed in every version.
	AuditPersonalFields Scope = "audit_personal_fields"
	// RunTasks: the task text of the person's runs.
	RunTasks Scope = "run_tasks"
	// RunOutputs: the stored output of the person's runs.
	RunOutputs Scope = "run_outputs"
	// Recordings: the person's session recordings. Opt-in: nothing deletes one
	// unless this scope is asked for.
	Recordings Scope = "recordings"
	// MaskCopies: the masking manifests of the person's runs, after their live
	// attaches and relays are fenced.
	MaskCopies Scope = "mask_copies"
)

// order is the sequence scopes run in whatever order they were asked: live
// consumers are fenced first, the data they could still reach is erased next,
// and the keys go last.
var order = []Scope{MaskCopies, RunOutputs, Recordings, RunTasks, AuditPersonalFields, Credentials}

// Scopes lists every scope in the order Orchestrate runs them.
func Scopes() []Scope { return slices.Clone(order) }

// Known reports whether s is a scope.
func Known(s Scope) bool { return slices.Contains(order, s) }

// ErrScopeUnknown is the error ParseScopes answers for a name that is no scope
// and for an empty list.
var ErrScopeUnknown = errors.New("erasure: scopes must be a non-empty list of known names")

// ParseScopes turns names into scopes. An empty list or an unknown name is
// ErrScopeUnknown naming it, so nothing runs on a request that misspelt one.
func ParseScopes(names []string) ([]Scope, error) {
	if len(names) == 0 {
		return nil, ErrScopeUnknown
	}
	out := make([]Scope, 0, len(names))
	for _, n := range names {
		s := Scope(n)
		if !Known(s) {
			return nil, fmt.Errorf("%w: %q is not one of %s", ErrScopeUnknown, n, joinScopes(order))
		}
		out = append(out, s)
	}
	return out, nil
}

// Step erases one scope for person. It must be idempotent: a retry after a
// partial failure runs every step again, including the ones that finished. The
// detail it returns is carried to the caller in Report.Details.
type Step func(ctx context.Context, person string) (detail any, err error)

// Orchestrator runs steps. A scope with no step is not available on this
// server, and asking for it fails rather than reporting it erased.
type Orchestrator struct {
	Steps map[Scope]Step
}

// Report says what an Orchestrate call did.
type Report struct {
	// Done lists the scopes that finished, in the order they ran.
	Done []Scope
	// Details holds what each finished step returned.
	Details map[Scope]any
}

// IncompleteError is a partial failure: the scopes in Remaining are not done,
// the first of them because of Cause. Done lists the ones that are. A retry
// with the same scopes finishes the rest.
type IncompleteError struct {
	Remaining []Scope
	Done      []Scope
	Cause     error
}

func (e *IncompleteError) Error() string {
	return fmt.Sprintf("erasure incomplete, scopes left: %s: %v", joinScopes(e.Remaining), e.Cause)
}

func (e *IncompleteError) Unwrap() error { return e.Cause }

// ErrNotAvailable is a scope this server has no way to erase.
var ErrNotAvailable = errors.New("erasure: this scope is not available on this server")

// Orchestrate erases person's records in scopes, once each, in the fixed order.
// It stops at the first scope that fails and returns an *IncompleteError naming
// that scope and every one after it, so the caller says exactly what is left.
// A scope that holds nothing for the person is a success: the call is
// idempotent, and a retry is the way to finish.
func (o *Orchestrator) Orchestrate(ctx context.Context, person string, scopes []Scope) (Report, error) {
	rep := Report{Details: map[Scope]any{}}
	if person == "" {
		return rep, errors.New("erasure: no person named")
	}
	asked := map[Scope]bool{}
	for _, s := range scopes {
		if !Known(s) {
			return rep, fmt.Errorf("%w: %q", ErrScopeUnknown, string(s))
		}
		asked[s] = true
	}
	if len(asked) == 0 {
		return rep, ErrScopeUnknown
	}
	var todo []Scope
	for _, s := range order {
		if asked[s] {
			todo = append(todo, s)
		}
	}
	for i, s := range todo {
		step := o.Steps[s]
		if step == nil {
			return rep, &IncompleteError{Remaining: todo[i:], Done: rep.Done, Cause: fmt.Errorf("%w: %s", ErrNotAvailable, s)}
		}
		detail, err := step(ctx, person)
		if err != nil {
			return rep, &IncompleteError{Remaining: todo[i:], Done: rep.Done, Cause: err}
		}
		rep.Done = append(rep.Done, s)
		rep.Details[s] = detail
	}
	return rep, nil
}

func joinScopes(ss []Scope) string {
	names := make([]string, len(ss))
	for i, s := range ss {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}
