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

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
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

// sealActor stores ev's human actor as its subject. A person with no identity
// row keeps their actor. When the subject cannot be had the actor waits under
// the pending key, as a sealed field does: the row is returned pending, and the
// placeholder PendingActor stands in the actor column.
func (s *Sealer) sealActor(ctx context.Context, ev types.AuditEvent) (types.AuditEvent, bool, error) {
	name := ev.Actor
	id, ok, err := s.actorSubject(ctx, name)
	if err == nil {
		if ok {
			ev.Actor = SubjectActorPrefix + id
		}
		return ev, false, nil
	}
	pk := s.pendingKey()
	if pk == nil {
		return ev, false, fmt.Errorf("audit seal: no subject for the actor and no pending key to hold the row: %w", err)
	}
	ct, serr := kek.Seal(pk, []byte(name), sealAAD(ev.ID.String(), ev.Action, actorPath, name, 0))
	if serr != nil {
		return ev, false, fmt.Errorf("audit seal: %w", serr)
	}
	m := map[string]json.RawMessage{}
	if len(ev.Data) > 0 {
		if jerr := json.Unmarshal(ev.Data, &m); jerr != nil || m == nil {
			return ev, false, fmt.Errorf("audit seal: the row's data is not an object, so the actor cannot wait in it: %v", jerr)
		}
	}
	m[PendingActorKey] = jsonString(pendingPrefix + b64([]byte(name)) + "." + b64(ct))
	m[PendingMarker] = json.RawMessage("true")
	data, merr := json.Marshal(m)
	if merr != nil {
		return ev, false, merr
	}
	ev.Data, ev.Actor = data, PendingActor
	return ev, true, nil
}

// resealActor is Reseal's step for the actor a pending row carries: it is
// opened and stored as its subject. An actor whose person was erased while the
// row waited is stored as its subject without a fresh key, so it reads erased,
// or as ErasedValue when there is no subject to store.
func (s *Sealer) resealActor(ctx context.Context, ev types.AuditEvent, pk []byte) (types.AuditEvent, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(ev.Data, &m) != nil {
		return ev, nil
	}
	raw, has := m[PendingActorKey]
	if !has {
		return ev, nil
	}
	if s.Subjects == nil {
		return ev, errors.New("audit reseal: a pending actor is spooled and the subject directory is not available")
	}
	str, _ := stringValue(raw)
	name, ct, ok := parsePending(str)
	if !ok {
		return ev, errors.New("audit reseal: the pending actor is malformed")
	}
	plain, err := kek.Open(pk, ct, sealAAD(ev.ID.String(), ev.Action, actorPath, name, 0))
	if err != nil {
		return ev, fmt.Errorf("audit reseal: the pending actor does not open under the pending key: %w", err)
	}
	actor := string(plain)
	principal, err := s.principalOf(ctx, actor)
	if err != nil {
		return ev, err
	}
	gone := false
	if s.GoneSince != nil {
		if gone, err = s.GoneSince(ctx, principal, ev.Time); err != nil {
			return ev, err
		}
	}
	var id string
	if gone {
		if id, ok, err = s.Subjects.AuditSubjectFor(ctx, principal); err != nil {
			return ev, err
		}
		actor = ErasedValue
		if ok {
			actor = SubjectActorPrefix + id
		}
	} else {
		if id, ok, err = s.actorSubject(ctx, actor); err != nil {
			return ev, err
		}
		if ok {
			actor = SubjectActorPrefix + id
		}
	}
	delete(m, PendingActorKey)
	data, err := json.Marshal(m)
	if err != nil {
		return ev, err
	}
	ev.Data, ev.Actor = data, actor
	return ev, nil
}

// renderActor is the actor a reader sees for a stored subject actor: the
// person, ErasedValue once a key of theirs was destroyed after the row was
// written, and the stored string for an id no identity row carries.
func (s *Sealer) renderActor(ctx context.Context, memo *keyMemo, ev types.AuditEvent) (string, error) {
	id := strings.TrimPrefix(ev.Actor, SubjectActorPrefix)
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
		return ev.Actor, nil
	case m.hasKey && m.destroyed.After(ev.Time):
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
