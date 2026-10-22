// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

// keyCustody is what the three key-custody rows are graded from.
type keyCustody struct {
	Domains       []string // the declared domain names, sorted
	PrincipalKeys bool
	RootKeyCreds  int // person-owned credentials still under the credential key
	ExternalCreds int // person-owned credentials kept in an external secret store, which no principal key seals
	Changes       keydomain.Changes
	Undeclared    []keydomain.UndeclaredDomain // domains assignments name that the file no longer declares
	Now           time.Time
}

// keyCustodyChecks are the key_domains, principal_keys and key_domain_changes rows (M5 S5). The
// changes row is amber while any assignment changed in the last 30 days: it is the mitigation for a
// database writer moving where a person's next keys are made.
func keyCustodyChecks(k keyCustody) []SetupCheck {
	domains := SetupCheck{ID: "key_domains", Label: "Key domains", Status: "info",
		Detail: "Only default: everyone's keys use this deployment's credential key."}
	if n := len(k.Domains); n > 0 {
		noun := "key domains, each proven at boot: "
		if n == 1 {
			noun = "key domain, proven at boot: "
		}
		domains.Status = "ok"
		domains.Detail = fmt.Sprintf("%d %s%s.", n, noun, strings.Join(k.Domains, ", "))
	}
	var undeclared []SetupCheck
	if len(k.Undeclared) > 0 {
		parts := make([]string, len(k.Undeclared))
		for i, u := range k.Undeclared {
			parts[i] = fmt.Sprintf("%s (%d)", u.Domain, u.Assignments)
		}
		undeclared = []SetupCheck{{ID: "key_domain_assignments", Label: "Key domain assignments", Status: "warn",
			Detail: "Key-domain assignments name domains that WARDYN_KEY_DOMAINS_FILE does not declare: " + strings.Join(parts, ", ") +
				". Every person such an assignment covers fails to get a key.",
			Fix: "Declare each domain in WARDYN_KEY_DOMAINS_FILE again and restart, or assign those people to a declared domain."}}
	}
	pk := SetupCheck{ID: "principal_keys", Label: "Per-person keys"}
	switch {
	case !k.PrincipalKeys:
		pk.Status = "info"
		pk.Detail = "Off: credentials written now use this deployment's key, or the external secret store when one is set. Audit records use per-person keys either way."
	case k.RootKeyCreds > 0:
		pk.Status = "info"
		pk.Detail = fmt.Sprintf("On. %d stored credentials still use this deployment's key.", k.RootKeyCreds)
		pk.Fix = "Run wardynd -rewrap-principal-keys to re-seal them."
	case k.ExternalCreds > 0:
		verb := "are"
		if k.ExternalCreds == 1 {
			verb = "is"
		}
		pk.Status = "ok"
		pk.Detail = fmt.Sprintf("On. Every stored credential held here is sealed under its owner's key. %d %s kept in the external secret store instead, outside these keys.", k.ExternalCreds, verb)
	default:
		pk.Status = "ok"
		pk.Detail = "On. Every stored credential is sealed under its owner's key."
	}
	ch := SetupCheck{ID: "key_domain_changes", Label: "Key domain changes", Status: "ok",
		Detail: "No key-domain assignment changed in the last 30 days."}
	if n := k.Changes.Count; n > 0 {
		s := "s"
		if n == 1 {
			s = ""
		}
		ch.Status = "warn"
		ch.Detail = fmt.Sprintf("%d key-domain assignment change%s in the last 30 days, the latest %s by %s. Each one moves where that person's next keys are made.",
			n, s, keyChangeWhen(k.Now, k.Changes.Latest), keyChangeBy(k.Changes.LatestBy))
		ch.Fix = "Check each in Audit: key_domain.assignment.set and key_domain.assignment.delete."
	}
	return slices.Concat([]SetupCheck{domains}, undeclared, []SetupCheck{pk, ch})
}

// keyChangeBy names who made a change. Under WARDYN_AUDIT_SEAL=full every human
// actor is stored as a "subject:<id>" pseudonym, which is never resolved here,
// so it reads as a phrase instead of an opaque id.
func keyChangeBy(actor string) string {
	if strings.HasPrefix(actor, audit.SubjectActorPrefix) {
		return "an admin whose name the audit seal hides"
	}
	return actor
}

// keyChangeWhen says how long ago t was, in days.
func keyChangeWhen(now, t time.Time) string {
	switch d := int(now.Sub(t) / (24 * time.Hour)); {
	case d <= 0:
		return "today"
	case d == 1:
		return "1 day ago"
	default:
		return fmt.Sprintf("%d days ago", d)
	}
}

// keyCustodyRows reads the key-custody inputs for an operator: handleSetupStatus redacts every check
// for anyone else, so they are not read for one. A read that fails leaves its row out rather than
// showing a guess.
func (s *Server) keyCustodyRows(ctx context.Context) []SetupCheck {
	svc := s.cfg.KeyDomains
	if svc == nil || !s.isOperator(ctx) {
		return nil
	}
	now := s.cfg.Now()
	k := keyCustody{Domains: svc.Declared(), PrincipalKeys: s.cfg.PrincipalKeys, Now: now}
	n, err := svc.RootKeyCredentials(ctx)
	if err != nil {
		return nil
	}
	k.RootKeyCreds = n
	if k.ExternalCreds, err = svc.ExternalCredentials(ctx); err != nil {
		return nil
	}
	if k.Changes, err = svc.ChangesSince(ctx, now.Add(-30*24*time.Hour)); err != nil {
		return nil
	}
	if k.Undeclared, err = svc.Undeclared(ctx); err != nil {
		return nil
	}
	return keyCustodyChecks(k)
}
