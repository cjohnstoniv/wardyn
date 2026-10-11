// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A service identity's sponsor is a person, and every row the service writes names them as data.sponsor
// (#1508). Under SealFull the sponsor, like an actor, is stored as the person's subject, so the row hash
// covers an id and not a name, and after the sponsor's erasure the service's trail names nobody. The same
// step stores sponsor_from and sponsor_to on service_identity.* rows, and target on invitation.* rows.
// A person with no identity row keeps their name, as an actor does; a sponsor always has one. The value
// waits under the pending key, as an actor does, when the subject cannot be had.

// sponsorFieldNames are the data fields of ev that can hold a person who is neither its actor nor its
// target column. "sponsor" is checked on every row because the recorder chain stamps it on all of a
// service request's rows, whichever writer made them.
func sponsorFieldNames(ev types.AuditEvent) []string {
	names := []string{"sponsor"}
	if strings.HasPrefix(ev.Action, "service_identity.") {
		names = append(names, "sponsor_from", "sponsor_to")
	}
	if strings.HasPrefix(ev.Action, "invitation.") {
		names = append(names, "target")
	}
	return names
}

// sponsorsIn is ev's data object and the sponsor fields it holds as non-empty strings.
func sponsorsIn(ev types.AuditEvent) (map[string]json.RawMessage, []string) {
	if len(ev.Data) == 0 || !bytes.Contains(ev.Data, []byte(`"sponsor`)) && !bytes.Contains(ev.Data, []byte(`"target"`)) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(ev.Data, &m) != nil || m == nil {
		return nil, nil
	}
	var held []string
	for _, n := range sponsorFieldNames(ev) {
		if v, ok := stringValue(m[n]); ok && v != "" {
			held = append(held, n)
		}
	}
	return m, held
}

// SealsSponsor reports whether ev holds a sponsor field SealFull stores as a subject.
func SealsSponsor(ev types.AuditEvent) bool {
	_, held := sponsorsIn(ev)
	return len(held) > 0
}

// forgedSponsor reports a sponsor field that already begins with SubjectActorPrefix: no Seal wrote it,
// and a reader would render it as the person it names.
func forgedSponsor(ev types.AuditEvent) bool {
	m, held := sponsorsIn(ev)
	for _, n := range held {
		if v, _ := stringValue(m[n]); strings.HasPrefix(v, SubjectActorPrefix) {
			return true
		}
	}
	return false
}

func sponsorPath(name string) string { return "data." + name }

// sealSponsors stores each sponsor field of ev as its subject; pending reports a field parked under the
// pending key, which sends the row to the spool.
func (s *Sealer) sealSponsors(ctx context.Context, ev types.AuditEvent) (types.AuditEvent, bool, error) {
	m, held := sponsorsIn(ev)
	if !s.SealActor || s.Subjects == nil || len(held) == 0 {
		return ev, false, nil
	}
	pending := false
	for _, n := range held {
		name, _ := stringValue(m[n])
		id, ok, err := s.actorSubject(ctx, name)
		if err == nil {
			if ok {
				m[n] = jsonString(SubjectActorPrefix + id)
			}
			continue
		}
		pk := s.pendingKey()
		if pk == nil {
			return ev, false, fmt.Errorf("audit seal: no subject for the %s and no pending key to hold the row: %w", sponsorPath(n), err)
		}
		if m[n], err = sealPending(pk, ev, sponsorPath(n), name, nil); err != nil {
			return ev, false, err
		}
		pending = true
	}
	if pending {
		m[PendingMarker] = json.RawMessage("true")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ev, false, err
	}
	ev.Data = data
	return ev, pending, nil
}

// resealSponsors is Reseal's step for the sponsor fields a pending row carries.
func (s *Sealer) resealSponsors(ctx context.Context, ev types.AuditEvent, pk []byte) (types.AuditEvent, error) {
	var m map[string]json.RawMessage
	if len(ev.Data) == 0 || json.Unmarshal(ev.Data, &m) != nil || m == nil {
		return ev, nil
	}
	changed := false
	for _, n := range sponsorFieldNames(ev) {
		str, ok := stringValue(m[n])
		if !ok || !strings.HasPrefix(str, pendingPrefix) {
			continue
		}
		pv, ok, err := openPending(pk, ev, sponsorPath(n), str)
		if !ok {
			return ev, fmt.Errorf("%w: the pending %s is malformed", ErrPendingUnopenable, sponsorPath(n))
		}
		if err != nil {
			return ev, fmt.Errorf("%w: the pending %s does not open under the pending key: %w", ErrPendingUnopenable, sponsorPath(n), err)
		}
		if s.Subjects == nil {
			return ev, errors.New("audit reseal: a pending sponsor is spooled and the subject directory is not available")
		}
		stored, err := s.resealedSubject(ctx, ev, pv.Subject)
		if err != nil {
			return ev, err
		}
		m[n], changed = jsonString(stored), true
	}
	if !changed {
		return ev, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ev, err
	}
	ev.Data = data
	return ev, nil
}

// unsealSponsors renders the subject strings in ev's sponsor fields back to the person, or ErasedValue once
// their key was destroyed.
func (s *Sealer) unsealSponsors(ctx context.Context, memo *keyMemo, ev types.AuditEvent) (json.RawMessage, bool, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(ev.Data, &m) != nil || m == nil {
		return ev.Data, false, nil
	}
	changed := false
	for _, n := range sponsorFieldNames(ev) {
		str, ok := stringValue(m[n])
		if !ok || !strings.HasPrefix(str, SubjectActorPrefix) {
			continue
		}
		person, err := s.renderSubject(ctx, memo, str, ev.Time)
		if err != nil {
			return nil, false, fmt.Errorf("audit unseal: %w", err)
		}
		m[n], changed = jsonString(person), true
	}
	if !changed {
		return ev.Data, false, nil
	}
	data, err := json.Marshal(m)
	return data, err == nil, err
}
