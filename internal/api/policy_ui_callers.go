// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxUIAppCallers bounds one inbound app's callers list. Callers are fixed at create and ride every
// run.policy.resolve envelope, so the bound keeps a policy from carrying a directory.
const maxUIAppCallers = 32

// validateUIAppCallers is validateUIApps' check of one app's inbound declaration. callers belong to an
// inbound app only; each entry is `user:<sub or email>`, `group:<name>` or `service:<name>`, already in the
// form the resolver matches (lower-cased, trimmed) and named once. `all` is not a caller: an inbound app
// is never open to every signed-in person. Names are shape-checked only, as capability grants are: a name
// nobody holds admits nobody. The ceiling clamp compares entries as strings, so a spelling that is not
// canonical is refused here rather than folded, which would let two spellings of one caller differ
// between a proposal and its ceiling.
func validateUIAppCallers(a types.UIApp) error {
	if len(a.Callers) == 0 {
		return nil
	}
	if !a.Inbound {
		return fmt.Errorf("callers is only meaningful with inbound: true")
	}
	if len(a.Callers) > maxUIAppCallers {
		return fmt.Errorf("callers: %d entries exceeds the maximum of %d", len(a.Callers), maxUIAppCallers)
	}
	seen := make(map[string]bool, len(a.Callers))
	for j, c := range a.Callers {
		if err := validateUIAppCaller(c); err != nil {
			return fmt.Errorf("callers[%d]: %w", j, err)
		}
		if seen[c] {
			return fmt.Errorf("callers[%d]: duplicate caller %q", j, c)
		}
		seen[c] = true
	}
	return nil
}

func validateUIAppCaller(c string) error {
	kind, name, _ := strings.Cut(c, ":")
	switch kind {
	case "user":
		// A user subject keeps non-ASCII (an IdP-issued sub), unfolded, as CanonicalUserSubject does.
		if name == "" || len(name) > maxCapabilityGrantFieldLen || !controlCharFree(name) || strings.ContainsRune(name, ' ') || types.CanonicalUserSubject(name) != name {
			return fmt.Errorf("invalid user caller %q (want user:<sub or email>, lower-case, no surrounding spaces)", c)
		}
	case "group":
		if canon, ok := types.CanonicalGroupSubject(name); !ok || canon != name || len(name) > maxCapabilityGrantFieldLen || !controlCharFree(name) {
			return fmt.Errorf("invalid group caller %q (want group:<name>, printable lower-case ASCII, no surrounding spaces)", c)
		}
	case "service":
		if !serviceNameRE.MatchString(name) {
			return fmt.Errorf("invalid service caller %q (want service:<name>, a service identity name: lower-case letters, digits and dashes, 3-63 chars, starting with a letter)", c)
		}
	default:
		return fmt.Errorf("invalid caller %q (want user:<sub or email>, group:<name> or service:<name>; \"all\" is not a caller)", c)
	}
	return nil
}

// portalCallerKeyRE reads the issuer key of a portal person's principal: delegate:person:<k>:<e>.
var portalCallerKeyRE = regexp.MustCompile(`^` + regexp.QuoteMeta(types.PortalPersonPrefix) + `([a-z2-7]{26}):.`)

// errUIAppCallerUnboundPortal is a user caller that names a portal person whose issuer has no binding.
var errUIAppCallerUnboundPortal = fmt.Errorf("a UI app caller names a portal person of an issuer that is not registered: %w", store.ErrDelegatePersonUnknownPortal)

// uiAppPortalKeys is the sorted, de-duplicated issuer keys of the portal people named as `user:` callers of
// apps. A caller that names a portal person but carries no well-formed key is errUIAppCallerUnboundPortal.
// validateUIAppCaller has already refused a spelling that is not canonical, so no folding happens here
// beyond the one CanonicalUserSubject gives every user subject.
func uiAppPortalKeys(apps []types.UIApp) ([]string, error) {
	var keys []string
	for _, a := range apps {
		for _, c := range a.Callers {
			kind, name, _ := strings.Cut(strings.TrimSpace(c), ":")
			if !strings.EqualFold(kind, "user") {
				continue
			}
			canon := types.CanonicalUserSubject(name)
			if !strings.HasPrefix(canon, types.PortalPersonPrefix) {
				continue
			}
			m := portalCallerKeyRE.FindStringSubmatch(canon)
			if m == nil {
				return nil, errUIAppCallerUnboundPortal
			}
			keys = append(keys, m[1])
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

// ruleUIAppCallers is the ruling for a policy spec's UI-app callers: a `user:delegate:person:<k>:...`
// caller whose issuer key has no binding is refused. It runs beside the spec validator at every site that
// stores a spec or starts a run, and the store write rules the same callers again in its own transaction,
// so a retirement that commits between the two is still caught.
func (s *Server) ruleUIAppCallers(ctx context.Context, spec types.RunPolicySpec) error {
	return s.ruleUIAppCallerApps(ctx, spec.UIApps)
}

// ruleUIAppCallerApps is ruleUIAppCallers over a list of apps, for the overlay of a unit and the like. It reads
// the bindings on a transaction that is always rolled back; a write that holds its own transaction calls
// ruleUIAppCallerAppsQ on it instead.
func (s *Server) ruleUIAppCallerApps(ctx context.Context, apps []types.UIApp) error {
	keys, err := uiAppPortalKeys(apps)
	if err != nil || len(keys) == 0 {
		return err
	}
	return s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error { return ruleUIAppPortalKeysQ(ctx, q, keys) })
}

// ruleUIAppCallerAppsQ is the ruling on the write's own transaction q: each binding is read FOR KEY SHARE and
// held to commit, so a retirement either commits first, and the write is refused, or waits for the write.
func ruleUIAppCallerAppsQ(ctx context.Context, q store.Querier, apps []types.UIApp) error {
	keys, err := uiAppPortalKeys(apps)
	if err != nil || len(keys) == 0 {
		return err
	}
	return ruleUIAppPortalKeysQ(ctx, q, keys)
}

func ruleUIAppPortalKeysQ(ctx context.Context, q store.Querier, keys []string) error {
	for _, k := range keys {
		bound, err := store.IssuerKeyBoundQ(ctx, q, k)
		if err != nil {
			return err
		}
		if !bound {
			return errUIAppCallerUnboundPortal
		}
	}
	return nil
}

// ruleUnitUIAppCallers is ruleUIAppCallerAppsQ for a governance unit's ceiling and overlay, as the 422 a
// unit write answers.
func ruleUnitUIAppCallers(ctx context.Context, q store.Querier, p types.GovernanceUnit) error {
	apps := slices.Clone(p.Ceiling.UIApps)
	if p.Overlay != nil && p.Overlay.UIApps != nil {
		apps = append(apps, *p.Overlay.UIApps...)
	}
	err := ruleUIAppCallerAppsQ(ctx, q, apps)
	if errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
		return writeRefusal(http.StatusUnprocessableEntity, reasonDelegatePersonUnknownPortal, "%s", delegatePersonUnknownPortalMsg)
	}
	return err
}

// uiAppsOfRawOverlay is the ui_apps entries of a launch's raw policy overlay, nil when it names none or does
// not decode (the overlay's own validation reports that).
func uiAppsOfRawOverlay(raw json.RawMessage) []types.UIApp {
	var o struct {
		UIApps []types.UIApp `json:"ui_apps"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &o) != nil {
		return nil
	}
	return o.UIApps
}

// uiAppCallerRefusal is the refusal for a ruling's error: 422 delegate_person_unknown_portal for the ruling,
// a server error for a read that failed.
func uiAppCallerRefusal(err error) *runRefusal {
	if errors.Is(err, store.ErrDelegatePersonUnknownPortal) {
		return runError(http.StatusUnprocessableEntity, reasonDelegatePersonUnknownPortal, delegatePersonUnknownPortalMsg)
	}
	return runServerError("rule the UI app callers", err)
}

// ruleUIAppCallersHTTP writes the refusal for a failed ruling of spec's callers and reports whether it did.
func (s *Server) ruleUIAppCallersHTTP(w http.ResponseWriter, r *http.Request, spec types.RunPolicySpec) bool {
	if err := s.ruleUIAppCallers(r.Context(), spec); err != nil {
		return uiAppCallerRefusal(err).write(s, w, r)
	}
	return false
}
