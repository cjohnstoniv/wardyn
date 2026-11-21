// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The rulings on admin-written person lists. A portal person (delegate:person:<k>:<e>) may be named by the
// lists that grant or restrict what their own runs may use (allow rows) and by none that confers an admin,
// sponsor, approver or console role (refuse rows). The ruling lives in the shared store write, not the HTTP
// handler: a held change keeps its subject in its payload, and approving it later calls the store write
// directly, without the handler's validator.
//
// An allow row also needs the principal's issuer key to have a binding, read FOR KEY SHARE inside the write's
// own transaction, so a retirement of the binding either commits first (the write finds none) or waits for the
// write (its census then finds the row). Removal is never ruled, so an old invalid row can always be deleted.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

type portalSubjectRuling int

const (
	// portalRefuse is a list that confers a role a portal person never holds.
	portalRefuse portalSubjectRuling = iota + 1
	// portalAllow is a list that grants or restricts what the person's own runs may use.
	portalAllow
)

// delegatePersonRulings names every list ruled here, by "table.column". The source guard
// TestPortalSubjectRuling_EveryWriteStatementRuled holds each write of these tables to a call of the ruling.
var delegatePersonRulings = map[string]portalSubjectRuling{
	"governance_unit_admins.subject":         portalRefuse,
	"template_group_admins.person":           portalRefuse,
	"role_mappings.value":                    portalRefuse,
	"remote_runners.people_subjects":         portalRefuse,
	"service_identities.sponsor":             portalRefuse,
	"capability_grants.subject":              portalAllow,
	"governance_memberships.subject":         portalAllow,
	"governance_restriction_members.subject": portalAllow,
	"user_drive_grants.subject":              portalAllow,
	"runner_pool_use_policies.subjects":      portalAllow,
}

// portalKeyInPrincipal is the issuer key of a canonical delegate:person:<k>:<e> principal.
var portalKeyInPrincipal = regexp.MustCompile(`^delegate:person:([a-z2-7]{26}):`)

// ruleDelegatePersonSubject rules one subject written to the list column ("table.column"). A subject that does
// not name a portal person passes untouched. One that does is refused with ErrDelegatePersonNotAllowed on a
// refuse list, and on an allow list with ErrDelegatePersonUnknownPortal unless its issuer key has a binding,
// which it reads FOR KEY SHARE on q: q must be the write's transaction for the lock to last to its commit. The
// subject is folded as the user-grant path folds it before it is read, so a mixed-case spelling is ruled too.
// Callers rule only a subject of type user; a group or service name that happens to start with the prefix is
// not a person.
func ruleDelegatePersonSubject(ctx context.Context, q rowQuerier, column, subject string) error {
	ruling, ok := delegatePersonRulings[column]
	if !ok {
		return fmt.Errorf("store: %s has no portal person ruling", column)
	}
	canon := types.CanonicalUserSubject(subject)
	if !strings.HasPrefix(canon, types.PortalPersonPrefix) {
		return nil
	}
	if ruling == portalRefuse {
		return ErrDelegatePersonNotAllowed
	}
	m := portalKeyInPrincipal.FindStringSubmatch(canon)
	if m == nil {
		return ErrDelegatePersonUnknownPortal
	}
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM portal_issuers WHERE issuer_key = $1 FOR KEY SHARE`, m[1]).Scan(&one)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrDelegatePersonUnknownPortal
	default:
		return fmt.Errorf("store: read issuer binding: %w", err)
	}
}

// uiAppCallerRE finds a user caller that names a portal person inside any stored policy document: the
// caller form is user:<principal>, folded to lower case.
var uiAppCallerRE = regexp.MustCompile(`user:delegate:person:([a-z2-7]{26}):`)

// ruleUIAppCallersQ is the ruling for the UI-app callers inside the policy documents a write stores (a
// policy spec, a unit's ceiling or overlay, a preset's request, a template revision's document). The callers
// live in JSON, so it reads each document as text and, for every issuer key a user caller names, takes that
// key's binding FOR KEY SHARE on q before the write, in key order so two writes never wait on each other. A
// key with no binding is ErrDelegatePersonUnknownPortal. q must be the write's transaction.
func ruleUIAppCallersQ(ctx context.Context, q rowQuerier, docs ...[]byte) error {
	var keys []string
	for _, d := range docs {
		for _, m := range uiAppCallerRE.FindAllStringSubmatch(strings.ToLower(string(d)), -1) {
			keys = append(keys, m[1])
		}
	}
	slices.Sort(keys)
	for _, k := range slices.Compact(keys) {
		var one int
		err := q.QueryRow(ctx, `SELECT 1 FROM portal_issuers WHERE issuer_key = $1 FOR KEY SHARE`, k).Scan(&one)
		switch {
		case err == nil:
		case errors.Is(err, pgx.ErrNoRows):
			return ErrDelegatePersonUnknownPortal
		default:
			return fmt.Errorf("store: read issuer binding: %w", err)
		}
	}
	return nil
}
