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
	// SealFull is SealFields and the human actor too: the actor column holds
	// SubjectActorPrefix and the person's subject id.
	SealFull SealMode = "full"
)

// SubjectActorPrefix begins the actor of a human row written under SealFull. A
// principal that starts with it is refused everywhere a person enters, so no
// one can sign in as, or be recorded under, another person's subject.
const SubjectActorPrefix = "subject:"

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
	// The free text the rejecting admin typed with the decision (gov4).
	"governance.change.reject": {{Path: "reason", Subject: SubjectActor}},
	// The person whose sessions were revoked: the target column's own name, repeated in data.
	"session.revoke": {{Path: "sub", Subject: SubjectTarget}},
	// The person a token mint was asked for, repeated from the target column.
	"person.token.create": {{Path: "target", Subject: SubjectTarget}},
	// The namespace owner an admin enumerated, repeated from the target column.
	"secret.list": {{Path: "secret_owner", Subject: SubjectTarget}},
	// The identity provider's sub of the person signing in, who is the actor on both rows: the
	// attached person on success, the sub's own person when the attach is ignored.
	"person.attach": {{Path: "sub", Subject: SubjectActor}},
	// The free text the security admin typed to justify a runner's trust change.
	"runner.trust.write": {{Path: "reason", Subject: SubjectActor}},
}

// SealLeafNames is the closed set of data key names a field may carry to be a
// candidate for sealing. It does not seal anything by itself.
var SealLeafNames = []string{"task", "prompt", "message", "reason", "comment", "note", "email", "display_name", "query", "command", "sub", "target", "secret_owner"}

// UnsealedActions are actions whose rows are never sealed, so the record of an
// erasure survives the key it destroys and a person's key is named in the
// clear. Pinned against sealFields by a test.
var UnsealedActions = []string{"person.erasure", "credential.erase", "principal_key.destroyed"}

// personTargetActions is the closed list of actions whose `target` column is a
// person's principal, so SealFull stores it as SubjectActorPrefix and the
// person's subject id, as it does a human actor. It only ever grows: a row sealed
// under an entry must stay readable after the entry would be removed. Each entry
// was read off its emit site, and an action whose target is sometimes a person
// and sometimes something else is NOT listed:
//   - ssh_key.delete (a key's fingerprint, or the principal an admin cleared),
//   - key_domain.assignment.set and .delete ("user:<principal>", "group:<name>", "all").
//
// Also absent:
//   - person.create, whose person is made before their first sign-in and so has
//     no identity row to take a subject from; a person.token.create for such a
//     person stays clear too,
//   - UnsealedActions (the record of an erasure names the person in the clear),
//   - every row whose target is an id (person.deprovision's and scim.user.write's
//     identity-row id, token.create's token id).
var personTargetActions = []string{
	"person.attach",            // peopleKeying.Attached and attachIgnored: the attached person's principal
	"person.token.create",      // handleMintPersonAPIToken: the principal in the path
	"delegation.exchange",      // handleTokenExchange: the verified subject of the person the portal acts for
	"session.revoke",           // handleRevokeSessions: the supplied sub ("*" names no person and is kept)
	"secret.list",              // handleListSecrets: the namespace owner an admin enumerated
	"auth.signin_unserialized", // lockLoginSupersede (harnesscred_supersede.go): the signing-in person, the login run's creator
}

// PersonTargetActions is personTargetActions, sorted.
func PersonTargetActions() []string {
	out := slices.Clone(personTargetActions)
	slices.Sort(out)
	return out
}

// SealsTarget reports whether the target of a row of action is a person stored
// as a subject under SealFull: a person-target action, and not one of
// UnsealedActions.
func SealsTarget(action string) bool {
	return slices.Contains(personTargetActions, action) && !slices.Contains(UnsealedActions, action)
}

// SealedFields lists the fields a row of action seals; nil for an action that
// seals none.
func SealedFields(action string) []SealField {
	return slices.Clone(sealFields[action])
}

// SealsAction reports whether any field of action is sealed.
func SealsAction(action string) bool { return len(sealFields[action]) > 0 }
