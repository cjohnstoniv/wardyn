// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type hybridSecrets map[string][]byte

func (m hybridSecrets) Get(_ context.Context, name string) ([]byte, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("secret %q not found: %w", name, pgx.ErrNoRows)
}

func (m hybridSecrets) Put(_ context.Context, name string, v []byte) error {
	m[name] = v
	return nil
}

// hybridStore is the local audit table (empty) and the org_federation row.
type hybridStore struct {
	mu      sync.Mutex
	cursor  int64
	head    int64
	revoked bool
}

func (s *hybridStore) cursorNow() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *hybridStore) FederationRevoked(context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked, nil
}
func (s *hybridStore) MarkFederationRevoked(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = true
	return nil
}
func (s *hybridStore) ResetFederation(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursor, s.revoked = 0, false
	return nil
}

func (s *hybridStore) ListAuditEventsAfterSeq(context.Context, int64, int) ([]types.FederatedAuditEvent, error) {
	return nil, nil
}
func (s *hybridStore) AuditHeadSeq(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head, nil
}
func (s *hybridStore) GetFederationCursor(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, nil
}
func (s *hybridStore) SetFederationCursor(_ context.Context, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursor = seq
	return nil
}

type hybridRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (r *hybridRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

// hybridOrg answers enrolment with a new device per call, and heartbeats.
type hybridOrg struct {
	mu      sync.Mutex
	enrols  []string
	devices []uuid.UUID
}

// seen returns the tokens redeemed and the devices issued so far.
func (o *hybridOrg) seen() ([]string, []uuid.UUID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.enrols), slices.Clone(o.devices)
}

func (o *hybridOrg) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if r.URL.Path != "/api/v1/devices/enrol" {
			_ = json.NewEncoder(w).Encode(types.DeviceAck{})
			return
		}
		var req types.DeviceEnrolRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		o.enrols = append(o.enrols, req.Token)
		id := uuid.New()
		o.devices = append(o.devices, id)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.DeviceEnrolResponse{DeviceID: id, Name: "alices-laptop", Token: "wdd_" + id.String()})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBootHybrid_NoOrgURLDoesNothing(t *testing.T) {
	secrets := hybridSecrets{}
	status, err := bootHybrid(context.Background(), t.Context(), "", "", secrets, &hybridStore{}, &hybridRecorder{})
	if err != nil || status != nil || len(secrets) != 0 {
		t.Fatalf("status=%v err=%v secrets=%v", status != nil, err, secrets)
	}
}

func TestBootHybrid_NoCredentialNoTokenRefuses(t *testing.T) {
	org := &hybridOrg{}
	_, err := bootHybrid(context.Background(), t.Context(), org.serve(t).URL, "", hybridSecrets{}, &hybridStore{}, &hybridRecorder{})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_ORG_ENROLMENT_TOKEN") {
		t.Fatalf("err = %v, want a refusal naming the token", err)
	}
	if enrols, _ := org.seen(); len(enrols) != 0 {
		t.Fatal("enrolled without a token")
	}
}

func TestBootHybrid_UnreachableOrgRefuses(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	secrets := hybridSecrets{}
	_, err := bootHybrid(context.Background(), t.Context(), srv.URL, "wde_x", secrets, &hybridStore{}, &hybridRecorder{})
	if err == nil || !strings.Contains(err.Error(), "enrolment at WARDYN_ORG_URL failed") {
		t.Fatalf("err = %v", err)
	}
	if len(secrets) != 0 {
		t.Fatal("a credential was stored after a failed enrolment")
	}
}

func TestBootHybrid_EnrolsOnceAndReEnrolsOnAFreshToken(t *testing.T) {
	org := &hybridOrg{}
	url := org.serve(t).URL
	secrets, st, rec := hybridSecrets{}, &hybridStore{cursor: 99}, &hybridRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	status, err := bootHybrid(context.Background(), ctx, url, "wde_first", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	enrols, devices := org.seen()
	var cred federation.Credential
	if err := json.Unmarshal(secrets[secretOrgDeviceCredential], &cred); err != nil {
		t.Fatal(err)
	}
	if len(enrols) != 1 || cred.DeviceID != devices[0] || cred.Token != "wdd_"+devices[0].String() ||
		cred.EnrolmentTokenSHA256 != federation.TokenSHA256("wde_first") {
		t.Fatalf("enrols=%v cred=%+v", enrols, cred)
	}
	if status().DeviceID != cred.DeviceID {
		t.Fatalf("status device = %v", status().DeviceID)
	}
	st.mu.Lock()
	if st.cursor != 0 {
		t.Errorf("cursor = %d, want 0 for a new device identity", st.cursor)
	}
	st.cursor, st.head = 7, 7
	st.mu.Unlock()
	if len(rec.events) != 1 || rec.events[0].Action != "device.local.enrol" || rec.events[0].Target != cred.DeviceID.String() {
		t.Fatalf("local rows = %+v", rec.events)
	}

	// A restart with the spent token still in secret.env keeps the credential.
	if _, err := bootHybrid(context.Background(), ctx, url, "wde_first", secrets, st, rec); err != nil {
		t.Fatal(err)
	}
	// And with no token at all.
	if _, err := bootHybrid(context.Background(), ctx, url, "", secrets, st, rec); err != nil {
		t.Fatal(err)
	}
	enrols, _ = org.seen()
	if len(enrols) != 1 || len(rec.events) != 1 || st.cursorNow() != 7 {
		t.Fatalf("restart re-enrolled: enrols=%d rows=%d cursor=%d", len(enrols), len(rec.events), st.cursorNow())
	}

	// A fresh token re-enrols as a new device and resets the cursor.
	if _, err := bootHybrid(context.Background(), ctx, url, "wde_second", secrets, st, rec); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(secrets[secretOrgDeviceCredential], &cred)
	enrols, devices = org.seen()
	if len(enrols) != 2 || cred.DeviceID != devices[1] || st.cursorNow() != 0 || len(rec.events) != 2 {
		t.Fatalf("re-enrol: enrols=%v cred=%v cursor=%d rows=%d", enrols, cred.DeviceID, st.cursorNow(), len(rec.events))
	}
}

// TestBootHybrid_RevokedLaptopBootsStillRefusing: the durable revoked mark is
// read at boot, before any org call — a revoked laptop restarted with the org
// unreachable publishes Revoked from the start. A fresh token re-enrols and
// clears it.
func TestBootHybrid_RevokedLaptopBootsStillRefusing(t *testing.T) {
	org := &hybridOrg{}
	srv := org.serve(t)
	secrets, st, rec := hybridSecrets{}, &hybridStore{}, &hybridRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := bootHybrid(context.Background(), ctx, srv.URL, "wde_first", secrets, st, rec); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.revoked = true // what Forwarder.revoke leaves behind
	st.mu.Unlock()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	status, err := bootHybrid(context.Background(), ctx, down.URL, "wde_first", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !status().Revoked {
		t.Fatal("a revoked laptop came back up enrolled")
	}
	status, err = bootHybrid(context.Background(), ctx, srv.URL, "wde_second", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if status().Revoked || st.revoked {
		t.Fatal("re-enrolment did not clear the revoked mark")
	}
}
