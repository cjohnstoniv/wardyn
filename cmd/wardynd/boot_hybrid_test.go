// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type hybridSecrets map[string][]byte

func (m hybridSecrets) Get(_ context.Context, name string) ([]byte, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("secret %q not found: %w", name, secretstore.ErrNotFound)
}

func (m hybridSecrets) Put(_ context.Context, name string, v []byte) error {
	m[name] = v
	return nil
}

// failingPutSecrets fails every Put while failPut is set, otherwise behaves
// like hybridSecrets. It isolates loadOrCreateSecret's Put failure from the
// Get failure paths hybridSecrets already covers.
type failingPutSecrets struct {
	mu      sync.Mutex
	data    map[string][]byte
	failPut bool
}

func newFailingPutSecrets() *failingPutSecrets { return &failingPutSecrets{data: map[string][]byte{}} }

func (m *failingPutSecrets) Get(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.data[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("secret %q not found: %w", name, secretstore.ErrNotFound)
}

func (m *failingPutSecrets) Put(_ context.Context, name string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPut {
		return errors.New("put failed")
	}
	m.data[name] = v
	return nil
}

// nthPutFailsSecrets fails only its Nth call to Put (1-indexed, failPutN==0
// means never), otherwise behaving like hybridSecrets. It isolates a single
// Put failure at a specific point in a multi-Put sequence — e.g. the SECOND
// Put in one bootHybrid call, the one that clears ResetPending after the
// reset and the local audit row are both durable — from the first Put (the
// one loadOrCreateSecret makes to store the freshly generated credential),
// which failingPutSecrets above already covers by failing every Put.
type nthPutFailsSecrets struct {
	mu       sync.Mutex
	data     map[string][]byte
	puts     int
	failPutN int
}

func newNthPutFailsSecrets(failPutN int) *nthPutFailsSecrets {
	return &nthPutFailsSecrets{data: map[string][]byte{}, failPutN: failPutN}
}

func (m *nthPutFailsSecrets) Get(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.data[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("secret %q not found: %w", name, secretstore.ErrNotFound)
}

func (m *nthPutFailsSecrets) Put(_ context.Context, name string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if m.puts == m.failPutN {
		return errors.New("put failed")
	}
	m.data[name] = v
	return nil
}

func (m *nthPutFailsSecrets) stored(name string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[name]
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

// TestBootHybrid_FailedPutLeavesTheRevokedMarkSet: Put-then-Reset (F2) — a
// re-enrolling laptop must not have its revoked mark cleared before the new
// credential is durably stored. If storing it fails, the boot refuses and the
// mark from the prior revocation stays set.
func TestBootHybrid_FailedPutLeavesTheRevokedMarkSet(t *testing.T) {
	org := &hybridOrg{}
	srv := org.serve(t)
	secrets := newFailingPutSecrets()
	secrets.failPut = true
	st, rec := &hybridStore{revoked: true}, &hybridRecorder{}
	if _, err := bootHybrid(context.Background(), t.Context(), srv.URL, "wde_first", secrets, st, rec); err == nil {
		t.Fatal("a failed Put must refuse the boot")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.revoked {
		t.Fatal("a failed Put cleared the revoked mark before the new credential was stored")
	}
}

// reviewRev08ResetStore fails ResetFederation once (failReset), then behaves
// like the wrapped hybridStore. It isolates the ResetFederation failure path
// from Put/Record failures the other doubles cover.
type reviewRev08ResetStore struct {
	*hybridStore
	failReset bool
}

func (s *reviewRev08ResetStore) ResetFederation(ctx context.Context) error {
	if s.failReset {
		return errors.New("temporary reset outage")
	}
	return s.hybridStore.ResetFederation(ctx)
}

// reviewRev08FlakyRecorder fails Record once (failNext), then behaves like the
// wrapped hybridRecorder. It isolates the device.local.enrol Record failure
// path (C-04 case (c)) from the ResetFederation failure path above.
type reviewRev08FlakyRecorder struct {
	*hybridRecorder
	failNext bool
}

func (r *reviewRev08FlakyRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if r.failNext {
		r.failNext = false
		return errors.New("temporary audit outage")
	}
	return r.hybridRecorder.Record(ctx, ev)
}

// TestReviewRev08ReenrolRecoversAfterResetFailure is C-04 case (b), the
// independent 0.8 review's finding F12: a ResetFederation failure right after
// the new credential is durably stored must not spend the enrolment token
// again, and the next boot must finish the reset (adopted verbatim from the
// review's evidence, /tmp/claude-1000/rev08/evidence/compatibility/
// review_rev08_hybrid_recovery_test.go).
func TestReviewRev08ReenrolRecoversAfterResetFailure(t *testing.T) {
	org := &hybridOrg{}
	server := org.serve(t)
	old := federation.Credential{DeviceID: uuid.New(), Token: "wdd_old",
		EnrolmentTokenSHA256: federation.TokenSHA256("wde_old")}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	secrets := hybridSecrets{secretOrgDeviceCredential: raw}
	st := &reviewRev08ResetStore{hybridStore: &hybridStore{cursor: 7, head: 7, revoked: true}, failReset: true}
	rec := &hybridRecorder{}
	_, err = bootHybrid(context.Background(), t.Context(), server.URL, "wde_fresh", secrets, st, rec)
	if err == nil || !strings.Contains(err.Error(), "reset federation state") {
		t.Fatalf("first boot should fail after durable credential write: %v", err)
	}
	cred, ok := parseOrgCredential(secrets[secretOrgDeviceCredential])
	if !ok || cred.DeviceID == old.DeviceID {
		t.Fatal("precondition: new credential was not durably stored before reset failed")
	}
	st.failReset = false
	status, err := bootHybrid(context.Background(), t.Context(), server.URL, "wde_fresh", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	got := status()
	if got.Revoked || got.AckedSeq != 0 {
		t.Errorf("restart kept prior device's state: revoked=%v acked_seq=%d; want active, cursor 0", got.Revoked, got.AckedSeq)
	}
	if enrols, _ := org.seen(); len(enrols) != 1 {
		t.Errorf("recovery tried to spend the single-use token again: %d enrolments", len(enrols))
	}
}

// TestBootHybrid_ResumesResetAfterCrashBeforeReset is C-04 case (a): a process
// that died after the new credential was durably stored with ResetPending
// true, but before ResetFederation ran, resumes the reset on the next boot —
// without calling the organisation's enrol route again.
func TestBootHybrid_ResumesResetAfterCrashBeforeReset(t *testing.T) {
	org := &hybridOrg{}
	srv := org.serve(t)
	cred := federation.Credential{DeviceID: uuid.New(), Token: "wdd_new",
		EnrolmentTokenSHA256: federation.TokenSHA256("wde_first"), ResetPending: true, Name: "alices-laptop"}
	raw, err := json.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}
	secrets := hybridSecrets{secretOrgDeviceCredential: raw}
	st := &hybridStore{cursor: 7, head: 7, revoked: true}
	rec := &hybridRecorder{}
	status, err := bootHybrid(context.Background(), t.Context(), srv.URL, "wde_first", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	got := status()
	if got.Revoked || got.AckedSeq != 0 {
		t.Errorf("resumed reset left stale state: revoked=%v acked_seq=%d; want active, cursor 0", got.Revoked, got.AckedSeq)
	}
	if len(rec.events) != 1 || rec.events[0].Action != "device.local.enrol" {
		t.Fatalf("local rows = %+v, want exactly one device.local.enrol", rec.events)
	}
	if enrols, _ := org.seen(); len(enrols) != 0 {
		t.Errorf("resuming a durably-stored reset spent the enrolment token: %d enrolments", len(enrols))
	}
	got2, ok := parseOrgCredential(secrets[secretOrgDeviceCredential])
	if !ok || got2.ResetPending {
		t.Fatal("ResetPending was not cleared once the resumed reset completed")
	}
}

// TestBootHybrid_ResumesResetAfterFlakyLocalRecord is C-04 case (c): a failure
// recording the local device.local.enrol row must not be lost or duplicated
// on retry, and must not spend the enrolment token again.
func TestBootHybrid_ResumesResetAfterFlakyLocalRecord(t *testing.T) {
	org := &hybridOrg{}
	srv := org.serve(t)
	secrets := hybridSecrets{}
	st := &hybridStore{}
	rec := &reviewRev08FlakyRecorder{hybridRecorder: &hybridRecorder{}, failNext: true}
	_, err := bootHybrid(context.Background(), t.Context(), srv.URL, "wde_first", secrets, st, rec)
	if err == nil || !strings.Contains(err.Error(), "device.local.enrol") {
		t.Fatalf("first boot should refuse after a failed local audit record: %v", err)
	}
	cred, ok := parseOrgCredential(secrets[secretOrgDeviceCredential])
	if !ok || !cred.ResetPending {
		t.Fatal("precondition: ResetPending should still be true after a failed Record")
	}
	status, err := bootHybrid(context.Background(), t.Context(), srv.URL, "wde_first", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	if status().AckedSeq != 0 || status().Revoked {
		t.Fatalf("resumed boot left stale state: %+v", status())
	}
	if len(rec.events) != 1 || rec.events[0].Action != "device.local.enrol" {
		t.Fatalf("local rows = %+v, want exactly one device.local.enrol", rec.events)
	}
	if enrols, _ := org.seen(); len(enrols) != 1 {
		t.Errorf("retry after a failed Record spent the enrolment token again: %d enrolments", len(enrols))
	}
}

// TestBootHybrid_RevokedAfterCompletionStaysRevokedOnRestart is C-04 case (d):
// once a reset has completed (ResetPending is durably false), a genuine
// revocation of that same identity must never be cleared by a later restart
// with the same (already-spent) token.
func TestBootHybrid_RevokedAfterCompletionStaysRevokedOnRestart(t *testing.T) {
	org := &hybridOrg{}
	srv := org.serve(t)
	secrets, st, rec := hybridSecrets{}, &hybridStore{}, &hybridRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := bootHybrid(context.Background(), ctx, srv.URL, "wde_first", secrets, st, rec); err != nil {
		t.Fatal(err)
	}
	// The organisation revokes this freshly-enrolled, reset-complete identity.
	st.mu.Lock()
	st.revoked = true
	st.mu.Unlock()
	status, err := bootHybrid(context.Background(), ctx, srv.URL, "wde_first", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !status().Revoked {
		t.Fatal("a genuine revocation after the reset completed was cleared by a restart with the same (spent) token")
	}
	if enrols, _ := org.seen(); len(enrols) != 1 {
		t.Fatalf("restart re-enrolled despite the same spent token: %d enrolments", len(enrols))
	}
}

// TestBootHybrid_ClearingPutFailureRefusesThenResumes pins C-04 crash window
// 3: a failure in the SECOND Put — the one that persists ResetPending=false
// once the reset and the local audit row are both durable — must still
// refuse the boot rather than run the forwarder with ResetPending durably
// true, and must resume cleanly on the next boot without spending the
// enrolment token again. If that Put's error were ever swallowed instead of
// refusing, the forwarder would run with ResetPending stuck true, and a later
// genuine revocation would be wiped by the next restart (case (d) again, via
// a different crash window).
func TestBootHybrid_ClearingPutFailureRefusesThenResumes(t *testing.T) {
	org := &hybridOrg{}
	srv := org.serve(t)
	// 1st Put (loadOrCreateSecret storing the freshly generated credential)
	// succeeds; 2nd Put (clearing ResetPending after the reset completes)
	// fails once.
	secrets := newNthPutFailsSecrets(2)
	st := &hybridStore{cursor: 7, head: 7, revoked: true}
	rec := &hybridRecorder{}

	// Boot 1: the clearing Put fails.
	status, err := bootHybrid(context.Background(), t.Context(), srv.URL, "wde_first", secrets, st, rec)
	if err == nil || !strings.Contains(err.Error(), "persist reset completion") {
		t.Fatalf("boot 1 err = %v, want a refusal naming persist reset completion", err)
	}
	if status != nil {
		t.Fatal("boot 1 must not return a status accessor after refusing")
	}
	cred, ok := parseOrgCredential(secrets.stored(secretOrgDeviceCredential))
	if !ok || !cred.ResetPending {
		t.Fatal("boot 1: the stored credential must still carry ResetPending=true after the clearing Put failed")
	}

	// Boot 2: the same (already-spent) token resumes and completes.
	status, err = bootHybrid(context.Background(), t.Context(), srv.URL, "wde_first", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	got := status()
	if got.Revoked || got.AckedSeq != 0 {
		t.Fatalf("boot 2 left stale state: revoked=%v acked_seq=%d; want active, cursor 0", got.Revoked, got.AckedSeq)
	}
	if enrols, _ := org.seen(); len(enrols) != 1 {
		t.Errorf("boot 2 spent the enrolment token again: %d enrolments", len(enrols))
	}
	if len(rec.events) < 2 {
		t.Fatalf("local rows = %+v, want at least 2 device.local.enrol rows (at-least-once across the retry)", rec.events)
	}
	for _, ev := range rec.events {
		if ev.Action != "device.local.enrol" {
			t.Fatalf("unexpected local row: %+v", ev)
		}
	}
	cred2, ok := parseOrgCredential(secrets.stored(secretOrgDeviceCredential))
	if !ok || cred2.ResetPending {
		t.Fatal("boot 2: ResetPending must be durably false once the resumed reset completed")
	}

	// Boot 3: a genuine revocation of the now-complete identity must stick.
	st.mu.Lock()
	st.revoked = true
	st.mu.Unlock()
	status, err = bootHybrid(context.Background(), t.Context(), srv.URL, "wde_first", secrets, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !status().Revoked {
		t.Fatal("boot 3: a genuine revocation was cleared by a restart")
	}
}
