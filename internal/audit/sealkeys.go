// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"fmt"
	"slices"
)

// SealPurpose is the principal_keys purpose a person's sealed audit fields sit
// under (subjectkey.PurposeAuditSeal; cmd/wardynd pins that they are equal).
const SealPurpose = "audit-seal"

// SealMode is WARDYN_AUDIT_SEAL.
type SealMode string

const (
	// SealOff stores every field in the clear (the default).
	SealOff SealMode = "off"
	// SealFields seals the personal data fields SealedFields lists.
	SealFields SealMode = "fields"
	// SealFull is SealFields and the human actor too (ar-l1.5); not yet served.
	SealFull SealMode = "full"
)

// ParseSealMode reads WARDYN_AUDIT_SEAL; empty is off.
func ParseSealMode(s string) (SealMode, error) {
	switch m := SealMode(s); m {
	case "":
		return SealOff, nil
	case SealOff, SealFields, SealFull:
		return m, nil
	}
	return "", fmt.Errorf("WARDYN_AUDIT_SEAL is %q; want off, fields or full", s)
}

// Subject says whose a sealed field is, and so whose key seals it.
type Subject int

const (
	// SubjectActor is the event's actor, when a person: the one who typed it.
	SubjectActor Subject = iota + 1
	// SubjectTarget is the event's target column: the person the row is about.
	SubjectTarget
)

// SealField is one personal field of one action's data: its JSON path (dots
// walk into nested objects) and whose it is.
type SealField struct {
	Path    string
	Subject Subject
}

// sealFields is the closed, action- and path-aware list of personal fields a
// row seals. A leaf name alone never seals: a `reason` that carries a refusal
// code is a machine value a SIEM rule and the console classify on, and stays
// clear. The list was finalised by reading every audit emit site against
// SealLeafNames (docs/AUDIT-ACTIONS.md, "Sealed fields", names each retained
// field and why the rest stay clear). It only ever grows: a row sealed under an
// entry must stay readable after the entry would be removed.
//
// Each field is sealed under the subject it describes, so an event about two
// people seals each person's field under that person's own key.
var sealFields = map[string][]SealField{
	// The email the admin recorded for the person being created.
	"person.create": {{Path: "email", Subject: SubjectTarget}},
	// The free text the decider typed with the decision.
	"approval.decide": {{Path: "reason", Subject: SubjectActor}},
}

// SealLeafNames is the closed set of data key names a field may carry to be a
// candidate for sealing. It does not seal anything by itself.
var SealLeafNames = []string{"task", "prompt", "message", "reason", "comment", "note", "email", "display_name", "query", "command"}

// UnsealedActions are actions whose rows are never sealed, so the record of an
// erasure survives the key it destroys and a person's key is named in the
// clear. Pinned against sealFields by a test.
var UnsealedActions = []string{"person.erasure", "credential.erase", "principal_key.destroyed"}

// SealedFields lists the fields a row of action seals; nil for an action that
// seals none.
func SealedFields(action string) []SealField {
	return slices.Clone(sealFields[action])
}

// SealsAction reports whether any field of action is sealed.
func SealsAction(action string) bool { return len(sealFields[action]) > 0 }
