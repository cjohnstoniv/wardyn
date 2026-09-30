// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
)

// TestBootHybrid_ChangedOrgURLReEnrols pins #1004.1: the stored credential is
// bound to the org URL it was enrolled at. Pointed at another URL the old
// bearer is not used, and a fresh token enrols there.
func TestBootHybrid_ChangedOrgURLReEnrols(t *testing.T) {
	first, second := &hybridOrg{}, &hybridOrg{}
	urlA, urlB := first.serve(t).URL, second.serve(t).URL
	secrets, st, rec := hybridSecrets{}, &hybridStore{}, &hybridRecorder{}
	ctx, hj := newHybridJoin(t)

	fwd, err := bootHybrid(context.Background(), ctx, urlA, "wde_first", unlocked(secrets), st, rec)
	hj.add(fwd)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := parseOrgCredential(secrets[secretOrgDeviceCredential])
	if stored.OrgURLSHA256 != federation.OrgURLSHA256(urlA) {
		t.Fatalf("stored org url hash = %q, want the hash of %s", stored.OrgURLSHA256, urlA)
	}

	// Same URL again (a trailing slash is the same URL): kept, no enrolment.
	fwd, err = bootHybrid(context.Background(), ctx, urlA+"/", "wde_first", unlocked(secrets), st, rec)
	hj.add(fwd)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := first.seen(); len(e) != 1 {
		t.Fatalf("same URL re-enrolled: %v", e)
	}

	// Another URL with no token: refuses, names the URL, sends the old bearer nowhere.
	_, err = bootHybrid(context.Background(), ctx, urlB, "", unlocked(secrets), st, rec)
	if err == nil || !strings.Contains(err.Error(), "different WARDYN_ORG_URL") {
		t.Fatalf("err = %v, want a refusal naming the changed URL", err)
	}
	if e, _ := second.seen(); len(e) != 0 {
		t.Fatalf("enrolled at the new URL without a token: %v", e)
	}
	if got, _ := parseOrgCredential(secrets[secretOrgDeviceCredential]); got.DeviceID != stored.DeviceID {
		t.Fatal("the stored credential was replaced by a refused boot")
	}

	// Another URL with a fresh token: re-enrols there, bound to the new URL.
	fwd, err = bootHybrid(context.Background(), ctx, urlB, "wde_second", unlocked(secrets), st, rec)
	hj.add(fwd)
	if err != nil {
		t.Fatal(err)
	}
	e, devices := second.seen()
	got, _ := parseOrgCredential(secrets[secretOrgDeviceCredential])
	if len(e) != 1 || got.DeviceID != devices[0] || got.OrgURLSHA256 != federation.OrgURLSHA256(urlB) {
		t.Fatalf("enrols=%v cred=%+v", e, got)
	}
}

// TestBootHybrid_CredentialWithoutOrgURLHashIsAdopted pins how a credential
// stored before the hash existed is handled: it is kept, not re-enrolled (its
// spent token cannot enrol again), and the URL configured at this boot is
// recorded on it. From then on it is bound like any other.
func TestBootHybrid_CredentialWithoutOrgURLHashIsAdopted(t *testing.T) {
	org := &hybridOrg{}
	url := org.serve(t).URL
	old := federation.Credential{DeviceID: uuid.New(), Token: "wdd_old",
		EnrolmentTokenSHA256: federation.TokenSHA256("wde_first"), Name: "alices-laptop"}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "org_url_sha256") {
		t.Fatal("fixture is not a pre-hash credential")
	}
	secrets := hybridSecrets{secretOrgDeviceCredential: raw}
	ctx, hj := newHybridJoin(t)

	fwd, err := bootHybrid(context.Background(), ctx, url, "wde_first", unlocked(secrets), &hybridStore{}, &hybridRecorder{})
	hj.add(fwd)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parseOrgCredential(secrets[secretOrgDeviceCredential])
	if !ok || got.DeviceID != old.DeviceID || got.Token != old.Token {
		t.Fatalf("legacy credential was replaced: %+v", got)
	}
	if got.OrgURLSHA256 != federation.OrgURLSHA256(url) {
		t.Fatalf("org url hash = %q, want it adopted from this boot's URL", got.OrgURLSHA256)
	}
	if e, _ := org.seen(); len(e) != 0 {
		t.Fatalf("a legacy credential was re-enrolled: %v", e)
	}

	// Adopted, so a different URL is now refused like any bound credential.
	other := (&hybridOrg{}).serve(t).URL
	if _, err := bootHybrid(context.Background(), ctx, other, "", unlocked(secrets), &hybridStore{}, &hybridRecorder{}); err == nil ||
		!strings.Contains(err.Error(), "different WARDYN_ORG_URL") {
		t.Fatalf("err = %v, want the adopted binding to refuse a changed URL", err)
	}
}
