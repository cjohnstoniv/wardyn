// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Locks (DESIGN-TREE §5.3): locked_knobs on a unit, or on the Wardyn boundary, names the knobs no unit
// below it may change, for every writer and a narrowing included. A write that changes a knob locked
// above it, or that locks a knob a unit below states otherwise, answers 409 unit_knob_locked. The
// boundary's locks bind while it is in hard_limits, as its walls do. Restrictions are not subject to
// locks. Runs report the knobs a lock fixes as locked (markLocked, run_policy_provenance.go).
package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lockableKnobs is every knob a lock may name: the JSON names of a unit overlay's policy knobs and of
// its limits (DESIGN-TREE §3).
var lockableKnobs = func() map[string]bool {
	out := map[string]bool{}
	for _, t := range []reflect.Type{reflect.TypeFor[types.CeilingOverlay](), reflect.TypeFor[types.LimitsOverlay]()} {
		for i := range t.NumField() {
			if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
				out[name] = true
			}
		}
	}
	return out
}()

// boundaryLocker is the locker name a boundary lock's refusal and provenance carry.
const boundaryLocker = "the Wardyn boundary"

// normalizeLockedKnobs trims, de-duplicates and sorts a locked_knobs list; a name that is not a knob is
// the message.
func normalizeLockedKnobs(knobs []string) ([]string, string) {
	out := make([]string, 0, len(knobs))
	for _, k := range knobs {
		k = strings.TrimSpace(k)
		if !lockableKnobs[k] {
			return nil, fmt.Sprintf("locked_knobs: %q is not a knob a unit sets", k)
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out, ""
}

// jsonFields is v's JSON object as raw fields by name; absent fields are ones v leaves unset.
func jsonFields(vs ...any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, v := range vs {
		raw, err := json.Marshal(v)
		if err != nil {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		for k, f := range m {
			if lockableKnobs[k] {
				out[k] = f
			}
		}
	}
	return out
}

// statedKnobs is what a unit states of its own, knob by knob: a composed unit's overlay and overlay
// limits, a standalone unit's ceiling and limits. A knob it does not state it leaves to its parent.
func statedKnobs(u types.GovernanceUnit) map[string]json.RawMessage {
	if u.Composed() {
		return jsonFields(u.Overlay, u.OverlayLimits)
	}
	return jsonFields(u.Ceiling, u.Limits)
}

// sameRaw reports whether two raw JSON values decode equal.
func sameRaw(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// knobLock is the nearest lock on one knob: who holds it, who to ask, and the value it fixes (the
// locking unit's effective value, or the boundary's own; nil when it names none).
type knobLock struct {
	locker  string
	contact *policyref.Contact
	value   json.RawMessage
}

// locksOver is every knob locked for a unit placed under parent in all, nearest locker first: each unit
// from parent up, then the boundary in hard_limits.
func (s *Server) locksOver(all []types.GovernanceUnit, parent *uuid.UUID, b types.Boundary, limit int) map[string]knobLock {
	byID := unitsByID(all)
	out := map[string]knobLock{}
	seen := map[uuid.UUID]bool{}
	for cur := parent; cur != nil && !seen[*cur] && len(seen) <= limit; {
		row, ok := byID[*cur]
		if !ok {
			break
		}
		seen[*cur] = true
		if len(row.LockedKnobs) > 0 {
			var eff map[string]json.RawMessage
			if res := s.resolveForDiff(all, row.ID); res != nil {
				eff = jsonFields(res.Ceiling, res.Limits)
			}
			for _, k := range row.LockedKnobs {
				if _, held := out[k]; !held {
					out[k] = knobLock{locker: row.Name, contact: nonZeroContact(row.Contact), value: eff[k]}
				}
			}
		}
		cur = row.ParentID
	}
	if b.HardLimits() {
		own := jsonFields(b.Spec, b.Limits)
		for _, k := range b.LockedKnobs {
			if _, held := out[k]; !held {
				out[k] = knobLock{locker: boundaryLocker, contact: nonZeroContact(b.Contact), value: own[k]}
			}
		}
	}
	return out
}

// lockedRefusal is the 409 for a write that changes knob, locked by l.
func lockedRefusal(knob string, l knobLock) *unitWriteError {
	return writeRefusal(http.StatusConflict, reasonUnitKnobLocked, "%s is locked by %s; ask %s", knob, l.locker, contactPhrase(l))
}

// contactPhrase is who a refusal tells a person to ask about a lock: the locker's contact, else the
// people who can change it.
func contactPhrase(l knobLock) string {
	if c := l.contact; c != nil {
		if v := cmp.Or(c.Owner, c.Email, c.RequestURL); v != "" {
			return v
		}
	}
	if l.locker == boundaryLocker {
		return "a security admin"
	}
	return "an admin of " + l.locker
}

// checkUnitLocks refuses row p, about to be written over all, when it changes a knob locked above it
// (a narrowing included), or when it locks a knob a unit below it states with another value. A new or
// re-parented unit changes every knob it states, and a re-parented unit's descendants come under the
// new locks with what they state. A stated value equal to the locked one changes nothing.
func (s *Server) checkUnitLocks(p types.GovernanceUnit, res *ResolvedUnit, all []types.GovernanceUnit, b types.Boundary, limit int) error {
	cur, exists := unitsByID(all)[p.ID]
	moved := !exists || !sameJSON(cur.ParentID, p.ParentID)
	proposed := append(slices.DeleteFunc(slices.Clone(all), func(x types.GovernanceUnit) bool { return x.ID == p.ID }), p)
	locks := s.locksOver(proposed, p.ParentID, b, limit)
	if len(locks) > 0 {
		var old map[string]json.RawMessage
		if !moved {
			old = statedKnobs(cur)
		}
		for _, k := range slices.Sorted(maps.Keys(locks)) {
			v, states := statedKnobs(p)[k]
			if !states || sameRaw(v, locks[k].value) {
				continue
			}
			if was, had := old[k]; !had || !sameRaw(v, was) {
				return lockedRefusal(k, locks[k])
			}
		}
		if moved && exists {
			for _, d := range descendantsOf(proposed, p.ID) {
				dlocks := s.locksOver(proposed, d.ParentID, b, limit)
				for _, k := range slices.Sorted(maps.Keys(dlocks)) {
					if v, states := statedKnobs(d)[k]; states && !sameRaw(v, dlocks[k].value) {
						return lockedRefusal(k, dlocks[k])
					}
				}
			}
		}
	}
	var before []string
	if exists {
		before = cur.LockedKnobs
	}
	var eff map[string]json.RawMessage
	if res != nil {
		eff = jsonFields(res.Ceiling, res.Limits)
	}
	for _, k := range p.LockedKnobs {
		if slices.Contains(before, k) {
			continue
		}
		var differ []string
		for _, d := range descendantsOf(proposed, p.ID) {
			if v, states := statedKnobs(d)[k]; states && !sameRaw(v, eff[k]) {
				differ = append(differ, d.Name)
			}
		}
		if len(differ) > 0 {
			return writeRefusal(http.StatusConflict, reasonUnitKnobLocked,
				"%s cannot be locked: %s set it to another value; change them first", k, strings.Join(differ, ", "))
		}
	}
	return nil
}

// boundaryLockConflicts is the units that state a knob a boundary write newly locks with a value other
// than the boundary's: the boundary's 409 lists them. Only a boundary in hard_limits binds.
func boundaryLockConflicts(units []types.GovernanceUnit, before, after types.Boundary) []string {
	if !after.HardLimits() {
		return nil
	}
	own := jsonFields(after.Spec, after.Limits)
	var out []string
	for _, k := range after.LockedKnobs {
		if before.HardLimits() && slices.Contains(before.LockedKnobs, k) {
			continue
		}
		for _, u := range units {
			if v, states := statedKnobs(u)[k]; states && !sameRaw(v, own[k]) && !slices.Contains(out, k+": "+u.Name) {
				out = append(out, k+": "+u.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// locksRemoved reports whether a write takes a lock off a unit: a widening, so never exempt.
func locksRemoved(before, after []string) bool {
	return slices.ContainsFunc(before, func(k string) bool { return !slices.Contains(after, k) })
}
