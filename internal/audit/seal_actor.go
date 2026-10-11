// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The actor of a human row under SealFull is "subject:<uuid>", the id of the
// person's oldest principal_identities row. The row hash covers that string, so
// erasing the person (destroying their audit-seal key) leaves nothing in the
// row that names them and the chain still verifies. The id maps back to the
// person only while no key of theirs was destroyed after the row was written;
// a reader shows ErasedValue after. Seal makes sure the key exists when it
// writes the actor, so an erasure always has one to destroy.

// sealsActor reports whether ev's actor is stored as a subject: a human, named,
// not already a subject, and not an action whose row must stay readable after
// the erasure it records.
func (s *Sealer) sealsActor(ev types.AuditEvent) bool {
	return s.SealActor && s.Subjects != nil && ev.ActorType == types.ActorHuman && ev.Actor != "" &&
		!strings.HasPrefix(ev.Actor, SubjectActorPrefix) && !slices.Contains(UnsealedActions, ev.Action)
}

// ForgedSubject reports whether ev carries a subject string no Seal wrote: a
// human actor, the target of a person-target action, or a sponsor field (seal_sponsor.go), that already begins with
// SubjectActorPrefix. Unseal renders such a string as the person it names, so a
// caller who typed one (a path segment, a request body) could read an erased
// person's rows back; Seal refuses the row instead.
func ForgedSubject(ev types.AuditEvent) bool {
	return (ev.ActorType == types.ActorHuman && strings.HasPrefix(ev.Actor, SubjectActorPrefix)) ||
		(SealsTarget(ev.Action) && strings.HasPrefix(ev.Target, SubjectActorPrefix)) || forgedSponsor(ev)
}

// sealsTarget reports whether ev's target is stored as a subject: an action
// whose target is a person's principal (personTargetActions), named, not
// already a subject, and not an action whose row must stay readable after the
// erasure it records.
func (s *Sealer) sealsTarget(ev types.AuditEvent) bool {
	return s.SealActor && s.Subjects != nil && SealsTarget(ev.Action) && ev.Target != "" &&
		!strings.HasPrefix(ev.Target, SubjectActorPrefix)
}

// principalOf resolves a name an event carries (an email form, an entra:
// principal) to the person's principal.
func (s *Sealer) principalOf(ctx context.Context, name string) (string, error) {
	if s.Resolve == nil {
		return name, nil
	}
	return s.Resolve(ctx, name)
}

// actorSubject is the subject id for the person name resolves to, with their
// audit-seal key created when there is none. ok is false for a person with no
// identity row.
func (s *Sealer) actorSubject(ctx context.Context, name string) (id string, ok bool, err error) {
	principal, err := s.principalOf(ctx, name)
	if err != nil {
		return "", false, err
	}
	if id, ok, err = s.Subjects.AuditSubjectFor(ctx, principal); err != nil || !ok {
		return "", false, err
	}
	_, key, err := s.Keys.Current(ctx, principal, SealPurpose)
	if err != nil {
		return "", false, err
	}
	clear(key)
	return id, true, nil
}

// ActorSubject is the actor column a human row written outside the recorder
// chain stores under SealFull (the retention drop's event and anchor): the
// person's subject, with their audit-seal key created when there is none, as
// Seal stores it. A person with no identity row keeps their name.
func (s *Sealer) ActorSubject(ctx context.Context, principal string) (string, error) {
	if s.Subjects == nil {
		return "", errors.New("audit seal: the subject directory is not available")
	}
	id, ok, err := s.actorSubject(ctx, principal)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return principal, nil
	}
	return SubjectActorPrefix + id, nil
}

// sealActor stores ev's human actor as its subject. A person with no identity
// row keeps their actor. When the subject cannot be had the actor waits under
// the pending key, as a sealed field does: the row is returned pending, and the
// placeholder PendingActor stands in the actor column.
func (s *Sealer) sealActor(ctx context.Context, ev types.AuditEvent) (types.AuditEvent, bool, error) {
	id, ok, err := s.actorSubject(ctx, ev.Actor)
	if err == nil {
		if ok {
			ev.Actor = SubjectActorPrefix + id
		}
		return ev, false, nil
	}
	parked, perr := s.park(ev, PendingActorKey, actorPath, ev.Actor, err)
	if perr != nil {
		return ev, false, perr
	}
	parked.Actor = PendingActor
	return parked, true, nil
}

// sealTarget is sealActor for the target column of a person-target action.
func (s *Sealer) sealTarget(ctx context.Context, ev types.AuditEvent) (types.AuditEvent, bool, error) {
	id, ok, err := s.actorSubject(ctx, ev.Target)
	if err == nil {
		if ok {
			ev.Target = SubjectActorPrefix + id
		}
		return ev, false, nil
	}
	parked, perr := s.park(ev, PendingTargetKey, targetPath, ev.Target, err)
	if perr != nil {
		return ev, false, perr
	}
	parked.Target = PendingTarget
	return parked, true, nil
}

// park holds name, a column's person, in ev's data under dataKey sealed with the
// pending key, and marks the row pending; the caller swaps the column for its
// placeholder. cause is why no subject could be had.
func (s *Sealer) park(ev types.AuditEvent, dataKey, path, name string, cause error) (types.AuditEvent, error) {
	pk := s.pendingKey()
	if pk == nil {
		return ev, fmt.Errorf("audit seal: no subject for the %s and no pending key to hold the row: %w", path, cause)
	}
	sealed, err := sealPending(pk, ev, path, name, nil)
	if err != nil {
		return ev, err
	}
	m := map[string]json.RawMessage{}
	if len(ev.Data) > 0 {
		if jerr := json.Unmarshal(ev.Data, &m); jerr != nil || m == nil {
			return ev, fmt.Errorf("audit seal: the row's data is not an object, so the %s cannot wait in it: %v", path, jerr)
		}
	}
	m[dataKey] = sealed
	m[PendingMarker] = json.RawMessage("true")
	if ev.Data, err = json.Marshal(m); err != nil {
		return ev, err
	}
	return ev, nil
}

// resealActor is Reseal's step for the actor a pending row carries: it is
// opened and stored as its subject. An actor whose person was erased while the
// row waited is stored as its subject without a fresh key, so it reads erased,
// or as ErasedValue when there is no subject to store.
func (s *Sealer) resealActor(ctx context.Context, ev types.AuditEvent, pk []byte) (types.AuditEvent, error) {
	data, stored, has, err := s.resealName(ctx, ev, pk, PendingActorKey, actorPath)
	if err != nil || !has {
		return ev, err
	}
	ev.Data, ev.Actor = data, stored
	return ev, nil
}

// resealTarget is resealActor for the target column.
func (s *Sealer) resealTarget(ctx context.Context, ev types.AuditEvent, pk []byte) (types.AuditEvent, error) {
	data, stored, has, err := s.resealName(ctx, ev, pk, PendingTargetKey, targetPath)
	if err != nil || !has {
		return ev, err
	}
	ev.Data, ev.Target = data, stored
	return ev, nil
}

// resealName opens the person waiting in ev's data under dataKey and returns the
// data without it and the column value to store; has is false when the row
// carries none.
func (s *Sealer) resealName(ctx context.Context, ev types.AuditEvent, pk []byte, dataKey, path string) (data json.RawMessage, stored string, has bool, err error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(ev.Data, &m) != nil {
		return nil, "", false, nil
	}
	raw, has := m[dataKey]
	if !has {
		return nil, "", false, nil
	}
	if s.Subjects == nil {
		return nil, "", false, fmt.Errorf("audit reseal: a pending %s is spooled and the subject directory is not available", path)
	}
	str, _ := stringValue(raw)
	// Neither failure heals by waiting, so neither defers: the drain counts the
	// line toward quarantine, as it does a pending field, instead of holding
	// every line behind it.
	pv, ok, err := openPending(pk, ev, path, str)
	if !ok {
		return nil, "", false, fmt.Errorf("%w: the pending %s is malformed", ErrPendingUnopenable, path)
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("%w: the pending %s does not open under the pending key: %w", ErrPendingUnopenable, path, err)
	}
	name, err := s.resealedSubject(ctx, ev, pv.Subject)
	if err != nil {
		return nil, "", false, err
	}
	delete(m, dataKey)
	if data, err = json.Marshal(m); err != nil {
		return nil, "", false, err
	}
	return data, name, true, nil
}

// resealedSubject is the value stored for the person name a pending field waited for: their subject, or
// the subject without a fresh key (ErasedValue when there is none) for a person erased while it waited.
func (s *Sealer) resealedSubject(ctx context.Context, ev types.AuditEvent, name string) (string, error) {
	principal, err := s.principalOf(ctx, name)
	if err != nil {
		return "", err
	}
	gone := false
	if s.GoneSince != nil {
		if gone, err = s.GoneSince(ctx, principal, ev.Time); err != nil {
			return "", err
		}
	}
	var id string
	var ok bool
	if gone {
		if id, ok, err = s.Subjects.AuditSubjectFor(ctx, principal); err != nil {
			return "", err
		}
		name = ErasedValue
	} else if id, ok, err = s.actorSubject(ctx, name); err != nil {
		return "", err
	}
	if ok {
		name = SubjectActorPrefix + id
	}
	return name, nil
}

// renderSubject is what a reader sees for a stored subject string (an actor or
// a target): the person, ErasedValue once a key of theirs was destroyed after
// the row was written, and the stored string for an id no identity row carries.
func (s *Sealer) renderSubject(ctx context.Context, memo *keyMemo, stored string, at time.Time) (string, error) {
	id := strings.TrimPrefix(stored, SubjectActorPrefix)
	m, seen := memo.actors[id]
	if !seen {
		principal, found, err := s.Subjects.AuditPrincipalOf(ctx, id)
		if err != nil {
			return "", err
		}
		if found {
			m.principal = principal
			if m.destroyed, m.hasKey, err = s.Subjects.AuditKeyDestroyedAt(ctx, principal); err != nil {
				return "", err
			}
		}
		memo.actors[id] = m
	}
	switch {
	case m.principal == "":
		return stored, nil
	case m.hasKey && m.destroyed.After(at):
		return ErasedValue, nil
	}
	return m.principal, nil
}

// StoredActor implements ActorTranslator.
func (s *Sealer) StoredActor(ctx context.Context, name string) (string, bool, error) {
	if s.Subjects == nil || strings.HasPrefix(name, SubjectActorPrefix) {
		return "", false, nil
	}
	principal, err := s.principalOf(ctx, name)
	if err != nil {
		return "", false, err
	}
	id, ok, err := s.Subjects.AuditSubjectFor(ctx, principal)
	if err != nil || !ok {
		return "", false, err
	}
	return SubjectActorPrefix + id, true, nil
}
