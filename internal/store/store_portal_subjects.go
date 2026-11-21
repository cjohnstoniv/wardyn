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
	"bytes"
	"context"
	"encoding/json"
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

// userCallerPrefix starts a UI-app caller that names a person: user:<sub or email>.
const userCallerPrefix = "user:"

// uiAppCallerKeys decodes each document and returns the sorted, de-duplicated issuer keys of the portal people
// named as callers of a UI app. It inspects only the callers of the entries of a "ui_apps" array (the keys
// match in any letter case, as the decoders that consume the documents do), wherever in
// the document that array sits (a policy, a unit's ceiling or overlay, a launch request's policy overlay, a
// template's document), so any JSON spelling of a caller is read as the decoded string and free text such as a
// task or a description is never read. A caller that names a portal person but has no well-formed issuer key
// is ErrDelegatePersonUnknownPortal.
func uiAppCallerKeys(docs ...[]byte) ([]string, error) {
	var keys []string
	for _, d := range docs {
		if len(bytes.TrimSpace(d)) == 0 {
			continue
		}
		var doc any
		if err := json.Unmarshal(d, &doc); err != nil {
			return nil, fmt.Errorf("store: read a policy document for its callers: %w", err)
		}
		var walkErr error
		var walk func(v any)
		walk = func(v any) {
			switch t := v.(type) {
			case map[string]any:
				for k, child := range t {
					if strings.EqualFold(k, "ui_apps") {
						if apps, ok := child.([]any); ok {
							for _, a := range apps {
								if app, ok := a.(map[string]any); ok {
									for ak, av := range app {
										callers, ok := av.([]any)
										if !ok || !strings.EqualFold(ak, "callers") {
											continue
										}
										for _, c := range callers {
											if str, ok := c.(string); ok {
												key, err := portalKeyOfCaller(str)
												if err != nil {
													walkErr = err
												} else if key != "" {
													keys = append(keys, key)
												}
											}
										}
									}
								}
							}
						}
					}
					walk(child)
				}
			case []any:
				for _, child := range t {
					walk(child)
				}
			}
		}
		walk(doc)
		if walkErr != nil {
			return nil, walkErr
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

// portalKeyOfCaller is the issuer key of a user caller that names a portal person, "" for any other caller.
func portalKeyOfCaller(caller string) (string, error) {
	c := strings.TrimSpace(caller)
	if len(c) < len(userCallerPrefix) || !strings.EqualFold(c[:len(userCallerPrefix)], userCallerPrefix) {
		return "", nil
	}
	canon := types.CanonicalUserSubject(c[len(userCallerPrefix):])
	if !strings.HasPrefix(canon, types.PortalPersonPrefix) {
		return "", nil
	}
	m := portalKeyInPrincipal.FindStringSubmatch(canon)
	if m == nil || len(canon) == len(m[0]) {
		return "", ErrDelegatePersonUnknownPortal
	}
	return m[1], nil
}

// ruleUIAppCallersQ is the ruling for the UI-app callers inside the policy documents a write stores (a
// policy spec, a unit's ceiling or overlay, a preset's request, a template revision's document). For every
// issuer key a user caller names it takes that key's binding FOR KEY SHARE on q before the write, in key
// order so two writes never wait on each other. A key with no binding is ErrDelegatePersonUnknownPortal. q
// must be the write's transaction.
func ruleUIAppCallersQ(ctx context.Context, q rowQuerier, docs ...[]byte) error {
	keys, err := uiAppCallerKeys(docs...)
	if err != nil {
		return err
	}
	for _, k := range keys {
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
