// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNodeRecordAttestationRules pins the organisation's half of the node record rules: after a recorded
// name reuse only "node powered off" is taken, and "no prior holder" is for the no-association path only.
func TestNodeRecordAttestationRules(t *testing.T) {
	for _, c := range []struct {
		name        string
		attestation string
		held        store.RunContainmentPendingRow
		refused     string
	}{
		{"powered off on a causal run", types.NodeRecordPoweredOff, store.RunContainmentPendingRow{}, ""},
		{"powered off after a reuse", types.NodeRecordPoweredOff, store.RunContainmentPendingRow{NameReused: true}, ""},
		{"no prior holder on the no-association path", types.NodeRecordNoPriorHolder, store.RunContainmentPendingRow{NoCausalAssociation: true}, ""},
		{"no prior holder after a reuse", types.NodeRecordNoPriorHolder, store.RunContainmentPendingRow{NameReused: true, NoCausalAssociation: true}, "node name was reused"},
		{"no prior holder on a causal run", types.NodeRecordNoPriorHolder, store.RunContainmentPendingRow{}, "only for a run with no causal association"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := nodeRecordAttestationRefusal(c.attestation, c.held)
			if (c.refused == "") != (got == "") || !strings.Contains(got, c.refused) {
				t.Fatalf("refusal = %q, want one containing %q", got, c.refused)
			}
		})
	}
}

// TestNodeRecordNeedsTwoPeople: a recorded reuse and the no-association path always need two people,
// whatever the four-eyes setting; a causal run needs two only under four-eyes.
func TestNodeRecordNeedsTwoPeople(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "")
	for name, c := range map[string]struct {
		held store.RunContainmentPendingRow
		want bool
	}{
		"causal":         {store.RunContainmentPendingRow{}, false},
		"name reused":    {store.RunContainmentPendingRow{NameReused: true}, true},
		"no association": {store.RunContainmentPendingRow{NoCausalAssociation: true}, true},
	} {
		if got := nodeRecordNeedsTwo(c.held); got != c.want {
			t.Errorf("%s: needs two = %v, want %v", name, got, c.want)
		}
	}
	t.Setenv(envGovernanceSecondHuman, "true")
	if !nodeRecordNeedsTwo(store.RunContainmentPendingRow{}) {
		t.Error("a causal run's record under four-eyes needs one person")
	}
}

// TestNodeRecordApproverIsPseudonymous: the runner tells people apart by case-folded principal and email,
// carried as keyed pseudonyms: one person is one value under one key, two people are two, the value is not
// the plain SHA-256 anyone could recompute from an email, and a server with no key refuses.
func TestNodeRecordApproverIsPseudonymous(t *testing.T) {
	key := bytes.Repeat([]byte{7}, kek.DEKSize)
	s := &Server{cfg: Config{RunConfigKey: key}}
	approver := func(srv *Server, principal, email string) runner.NodeRecordApprover {
		t.Helper()
		got, err := srv.nodeRecordApprover(principal, email)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a, b := approver(s, "Sub-Alice", "Alice@Corp.Example"), approver(s, " sub-alice", "alice@corp.example ")
	if a != b || !a.Human || a.Principal == "" || strings.Contains(a.Principal+a.Email, "alice") {
		t.Fatalf("one person gave %+v and %+v", a, b)
	}
	plain := sha256.Sum256([]byte("alice@corp.example"))
	if strings.Contains(a.Email, hex.EncodeToString(plain[:])) {
		t.Fatalf("email pseudonym %q is the unkeyed SHA-256", a.Email)
	}
	if bob := approver(s, "sub-bob", "bob@corp.example"); bob.Principal == a.Principal || bob.Email == a.Email {
		t.Fatalf("two people share a pseudonym: %+v %+v", a, bob)
	}
	if other := approver(&Server{cfg: Config{RunConfigKey: bytes.Repeat([]byte{8}, kek.DEKSize)}}, "sub-alice", "alice@corp.example"); other.Email == a.Email {
		t.Fatal("the pseudonym does not depend on the server's key")
	}
	if approver(s, "sub-alice", "").Email != "" {
		t.Fatal("an absent email was filled in")
	}
	if _, err := (&Server{}).nodeRecordApprover("sub-alice", "alice@corp.example"); err == nil {
		t.Fatal("a server with no key made a pseudonym")
	}
}

// TestRelayIPFencedIsItsOwnFailure: a runner's relay_ip_fenced refusal, encoded on the runner and decoded
// from a REPLY on the org, ends the run with its own reason, and a message that only names it does not.
func TestRelayIPFencedIsItsOwnFailure(t *testing.T) {
	overWire := func(err error) error {
		t.Helper()
		b, merr := json.Marshal(runnerwire.Reply{Error: runnerwire.ErrorFor(err)})
		if merr != nil {
			t.Fatal(merr)
		}
		var rep runnerwire.Reply
		if uerr := json.Unmarshal(b, &rep); uerr != nil {
			t.Fatal(uerr)
		}
		return rep.Error.Err()
	}
	fenced := overWire(fmt.Errorf("k8s: proxy pod 10.244.1.7: %w", runner.ErrRelayIPFenced))
	if hint, reason := sandboxCreateFailure(fenced); reason != reasonRelayIPFenced || !strings.Contains(hint, "still held for an earlier run") {
		t.Fatalf("fenced = %q %q", hint, reason)
	}
	for _, err := range []error{
		overWire(errors.New("k8s: proxy pod 10.244.1.7: relay_ip_fenced: named, not coded")),
		&runnerwire.Error{Code: runnerwire.CodeRefused, Message: "relay_ip_fenced: x"},
		errors.New("relay_ip_fenced: not from the runner"),
	} {
		if _, reason := sandboxCreateFailure(err); reason == reasonRelayIPFenced {
			t.Errorf("%v was read as relay_ip_fenced", err)
		}
	}
}
