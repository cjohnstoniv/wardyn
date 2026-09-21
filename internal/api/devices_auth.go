// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The device-facing routes of hybrid enrolment (docs/design/0.8/PLAN.md): all
// an enrolled laptop's wardynd — a DAEMON, never a person — can reach. The
// anonymous first-boot exchange, the deviceAuth middleware, audit ingest and
// the heartbeat all live in this one file on purpose:
// TestDevices_DeviceRoutesNeverTouchHumanIdentity parses it and fails if it
// ever names a human-identity publisher, an operator predicate or a request-
// actor helper. A device request carries no OIDC human, and isOperator's
// no-human arm reads exactly that as the admin token — so these routes are
// safe only because they never ask. The admin half (mint, inventory, revoke)
// is in devices.go.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// deviceTokenPrefix marks the device bearer and enrolmentTokenPrefix the
// single-use token that buys one. Like apiTokenPrefix they are routing and
// leak-scanning hints, not the boundary — the 256 bits after them are. What
// matters about wdd_ is what it is NOT: it never carries wdn_, so
// apiTokenAuth hands it to adminAuth, which refuses it. A device token has no
// path to withHumanIdentity.
const (
	deviceTokenPrefix    = "wdd_"
	enrolmentTokenPrefix = "wde_"
)

// deviceAuthActor names the device routes' boundary on its auth.failed rows,
// beside http.go's adminAuthActor/internalAuthActor; deviceEnrolActor names
// the anonymous enrolment route on its device.enrol failure rows.
const (
	deviceAuthActor  = "wardyn/deviceAuth"
	deviceEnrolActor = "wardyn/deviceEnrol"
)

// The anonymous enrolment route's per-peer bucket. A laptop enrols once, so
// ten attempts and then one per ten seconds is generous for a person and
// useless for a guesser (the token is 256 bits; the bound is on store and
// audit load, not on guessing).
//
// ponytail: keyed on the TCP peer because RealIP is deliberately not installed
// (routes.go) — behind an ingress every laptop shares the ingress's bucket, so
// a large MDM rollout enrols at the sustained rate and retries on 429. A
// trusted-proxy allowlist is the upgrade if that ever bites.
const (
	enrolRatePerSec      = 0.1
	enrolBurst           = 10.0
	enrolLimiterMaxPeers = 4096
)

// Audit ingest bounds. The row cap is the forwarder's batch size; the byte cap
// is sized to it (8 MiB over 500 rows) rather than maxJSONBody, which one full
// batch of large rows can exceed — and a batch that can never fit is a
// forwarder stuck at one cursor forever.
const (
	maxDeviceIngestRows  = 500
	maxDeviceIngestBytes = 8 << 20
)

// deviceEnrolRequest / deviceEnrolResponse are POST /devices/enrol's wire
// shapes; deviceAck is what the ingest and heartbeat routes answer — the
// organisation's recorded cursor for the device, which the forwarder advances
// to.
type deviceEnrolRequest struct {
	Token string `json:"token"`
}

type deviceEnrolResponse struct {
	DeviceID uuid.UUID `json:"device_id"`
	Name     string    `json:"name"`
	Token    string    `json:"token"`
}

type deviceAck struct {
	AckedSeq int64 `json:"acked_seq"`
}

type deviceCtxKey struct{}

// deviceFromContext is the device deviceAuth resolved. It is the ONLY identity
// the device routes publish; nothing that reads a human (isOperator,
// actorFromRequest, capability resolution) looks at this key.
func deviceFromContext(ctx context.Context) (types.Device, bool) {
	d, ok := ctx.Value(deviceCtxKey{}).(types.Device)
	return d, ok
}

// deviceActor is how a device is named on the audit rows it causes.
func deviceActor(id uuid.UUID) string { return "device:" + id.String() }

// newBearer mints prefix + 256 random bits, hex. crypto/rand.Read never
// returns an error (go1.24+) — it crashes the process instead.
func newBearer(prefix string) string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return prefix + hex.EncodeToString(raw)
}

// peerKey is the enrolment limiter's key: the TCP peer's address, an IPv6 peer
// by its /64 — one subscriber's allocation, which would otherwise hand a single
// caller 2^64 fresh buckets.
func peerKey(remoteAddr string) string {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() != nil {
		return host
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// deviceAuth authenticates the device routes: a wdd_ bearer that resolves to a
// LIVE device (store.PG.GetDeviceByRaw filters revoked rows, so revoked and
// unknown are one 401) whose id is the path's {id}. Every other credential —
// an SSO session, the admin token, a wdn_ token, none — is 401: a human is
// not a device at any tier.
//
// A token on any id but its own is 404, byte-identical whether that id exists
// or not; a 403 would confirm another device is enrolled.
//
// Without store.DeviceStore it fails closed with 401 — no device can prove
// itself to a store that cannot hold one. A store FAILURE is 503, never 401:
// the forwarder reads 401 as "revoked", and a database outage must not look
// like a revocation.
func (s *Server) deviceAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ds, ok := s.cfg.Store.(store.DeviceStore)
		if !ok {
			s.auditAuthFailedAs(r, deviceAuthActor, "device_store_unavailable")
			writeError(w, http.StatusUnauthorized, "this deployment does not accept device credentials")
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			s.auditAuthFailedAs(r, deviceAuthActor, "missing_device_token")
			writeError(w, http.StatusUnauthorized, "missing device token")
			return
		}
		d, err := types.Device{}, store.ErrNotFound
		if strings.HasPrefix(tok, deviceTokenPrefix) {
			d, err = ds.GetDeviceByRaw(r.Context(), tok)
		}
		if errors.Is(err, store.ErrNotFound) {
			s.auditAuthFailedAs(r, deviceAuthActor, "invalid_device_token")
			writeError(w, http.StatusUnauthorized, "invalid device token")
			return
		}
		if err != nil {
			slog.ErrorContext(r.Context(), "api: device lookup failed; this request could not be authenticated",
				"error", err, "path", r.URL.Path)
			s.metrics.authStoreErrorInc()
			writeError(w, http.StatusServiceUnavailable, "device lookup failed")
			return
		}
		if id, perr := uuid.Parse(chi.URLParam(r, "id")); perr != nil || id != d.ID {
			writeError(w, http.StatusNotFound, "device not found")
			return
		}
		// Best effort, like TouchAPIToken: last-seen is inventory hygiene, never
		// an authorization input.
		_ = ds.TouchDevice(r.Context(), d.ID, s.cfg.Now().UTC())
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceCtxKey{}, d)))
	})
}

// handleDeviceEnrol is POST /api/v1/devices/enrol: a laptop's first boot trades
// a single-use enrolment token for its device credential, returned once and
// stored only as a hash.
//
// Anonymous by necessity — the token in the body is the only credential the
// laptop holds — so the per-peer limiter runs before anything else. Spent,
// expired and unknown tokens are one 401 (ConsumeEnrolmentToken does not tell
// them apart), and the failure row is rate-bound by the same process-wide
// bucket auth.failed uses, its drops counted in the same series.
//
// Consume and create are two statements: a failed create leaves the token
// spent and no device, and the answer is an admin re-mint, not a retry.
func (s *Server) handleDeviceEnrol(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now().UTC()
	if !s.enrolLimiter.allow(peerKey(r.RemoteAddr), now) {
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, "too many enrolment attempts from this address; retry later")
		return
	}
	ds, ok := s.cfg.Store.(store.DeviceStore)
	if !ok {
		// Content-free: an unauthenticated caller learns nothing about the backend.
		writeError(w, http.StatusNotImplemented, http.StatusText(http.StatusNotImplemented))
		return
	}
	var req deviceEnrolRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	t, ok, err := ds.ConsumeEnrolmentToken(r.Context(), req.Token, now)
	if err != nil {
		writeServerError(w, r, "consume enrolment token", err)
		return
	}
	if !ok {
		s.auditEnrolFailure(r, "invalid_enrolment_token")
		writeError(w, http.StatusUnauthorized, "enrolment token is not valid: unknown, expired or already used")
		return
	}
	raw := newBearer(deviceTokenPrefix)
	d, err := ds.CreateDevice(r.Context(), types.Device{ID: uuid.New(), Name: t.DeviceName, EnrolledBy: t.MintedBy}, raw)
	if err != nil {
		s.auditEnrolFailure(r, "device_create_failed")
		writeServerError(w, r, "create device", err)
		return
	}
	ev := s.auditEvent(nil, types.ActorSystem, deviceActor(d.ID), "device.enrol", d.ID.String(), "success",
		mustJSON(map[string]any{"name": d.Name, "enrolment_token_id": t.ID, "minted_by": t.MintedBy}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	writeJSON(w, http.StatusCreated, deviceEnrolResponse{DeviceID: d.ID, Name: d.Name, Token: raw})
}

// auditEnrolFailure writes one device.enrol failure row, bounded the way
// auth.failed is: identical consecutive refusals fold into one streak whose
// summary carries the count (enrolFailures), and every row that is written —
// opening row or summary — pays the process-wide auth-failure bucket
// (emitEnrolFailure).
//
// The streak key is the reason and the path, NOT the peer. auth.failed keys on
// the peer because on a loopback deployment it separates principals; on this
// anonymous route the peer is the caller's to rotate, and a key it can rotate
// is a fold it can defeat. Each opening row still carries its peer, and the
// per-peer limiter has already bounded each address.
func (s *Server) auditEnrolFailure(r *http.Request, reason string) {
	ev := s.auditEvent(nil, types.ActorSystem, deviceEnrolActor, "device.enrol", r.URL.Path, "failure",
		mustJSON(map[string]any{"reason": reason}))
	ev.SourceIP = r.RemoteAddr
	absorbed, summary := s.enrolFailures.fold(s, "", reason+" "+r.URL.Path, reason, ev, s.emitEnrolFailure)
	s.emitEnrolFailure(s.cfg.BaseCtx, summary)
	if absorbed {
		s.metrics.authFailedSuppressedInc()
		return
	}
	s.emitEnrolFailure(r.Context(), &ev)
}

// handleDeviceAuditIngest is POST /api/v1/devices/{id}/audit: one batch of the
// laptop's own chained audit rows, oldest first, answered 200 {acked_seq}.
// Verification — the claimed hashes recomputed in SQL, the link to what this
// organisation last recorded, no row under an org run — is
// store.PG.IngestDeviceAudit's, handed the TCP peer to record as source_ip in
// place of the device's claim. This handler bounds and shapes the batch and
// maps the outcome to what the forwarder acts on: 401 revoked (from
// deviceAuth, or revoked mid-request), 400 a row that cannot be stored as
// claimed, 422 a batch that does not extend the recorded chain or names an
// org run, 429 a push already in progress, 5xx retry. Every refusal after
// authentication is audited with its reason (bounded per device); a chain
// reset is accepted and audited as one.
func (s *Server) handleDeviceAuditIngest(w http.ResponseWriter, r *http.Request) {
	d, ok := deviceFromContext(r.Context())
	ds, isDS := s.cfg.Store.(store.DeviceStore)
	if !ok || !isDS {
		writeError(w, http.StatusUnauthorized, "invalid device token")
		return
	}
	// One push per device at a time, taken before the body is read. The hash
	// recompute holds a pool connection for as long as it runs, so without this
	// one device's concurrent pushes could occupy the pool the organisation's
	// own requests need. A real forwarder pushes one batch at a time and never
	// meets it; the 429 writes no audit row (nothing was refused on content).
	if _, busy := s.ingestInFlight.LoadOrStore(d.ID, struct{}{}); busy {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "a push from this device is already in progress; retry after it completes")
		return
	}
	defer s.ingestInFlight.Delete(d.ID)
	body, ok := readCappedBody(w, r, maxDeviceIngestBytes, "device audit batch")
	if !ok {
		s.auditIngestFailure(r, d, "invalid_body", 0)
		return
	}
	var rows []types.FederatedAuditEvent
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rows); err != nil {
		s.auditIngestFailure(r, d, "invalid_body", 0)
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(rows) > maxDeviceIngestRows {
		s.auditIngestFailure(r, d, "batch_too_large", len(rows))
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("batch exceeds %d rows", maxDeviceIngestRows))
		return
	}
	if msg := invalidFederatedRow(rows); msg != "" {
		s.auditIngestFailure(r, d, "invalid_row", len(rows))
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	res, err := ds.IngestDeviceAudit(r.Context(), d.ID, r.RemoteAddr, rows)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusUnauthorized, "invalid device token")
		return
	case errors.Is(err, store.ErrDeviceRevoked):
		// Revoked after deviceAuth admitted this request: the same 401 its
		// next request gets, recorded as the revocation it is.
		s.auditIngestFailure(r, d, "revoked", len(rows))
		writeError(w, http.StatusUnauthorized, "invalid device token")
		return
	case errors.Is(err, store.ErrFederatedRowInvalid):
		// A value Postgres cannot represent: the device's fault, so a 4xx the
		// forwarder stops on, never a 5xx it would retry forever.
		s.auditIngestFailure(r, d, "invalid_row", len(rows))
		writeError(w, http.StatusBadRequest, "a row holds a value that cannot be stored as claimed")
		return
	case errors.Is(err, store.ErrFederatedOrgRun):
		s.auditIngestFailure(r, d, "org_run", len(rows))
		writeError(w, http.StatusUnprocessableEntity, "a row names one of this organisation's own runs")
		return
	case errors.Is(err, store.ErrConflict):
		s.auditIngestFailure(r, d, "chain_mismatch", len(rows))
		writeError(w, http.StatusUnprocessableEntity, "batch does not extend this device's recorded audit chain")
		return
	case err != nil:
		s.auditIngestFailure(r, d, "store_error", len(rows))
		writeServerError(w, r, "ingest device audit", err)
		return
	}
	if res.Reset {
		// A genesis row after a recorded chain: a purge on the laptop. Accepted,
		// because refusing would strand every later row, and made evidence here
		// because a developer with root can purge.
		ev := s.auditEvent(nil, types.ActorSystem, deviceActor(d.ID), "device.audit.chain_reset", d.ID.String(), "success",
			mustJSON(map[string]any{"prior_seq": d.LastSeq, "prior_row_hash": d.LastRowHash, "accepted": res.Accepted}))
		ev.SourceIP = r.RemoteAddr
		s.recordAudit(r.Context(), ev)
	}
	acked := d.LastSeq
	if res.Accepted > 0 {
		acked = rows[len(rows)-1].Seq
	}
	writeJSON(w, http.StatusOK, deviceAck{AckedSeq: acked})
}

// invalidFederatedRow is the boundary check the store cannot give a useful
// answer to: seq must strictly increase (the store's idempotency skip reads
// the batch in order), and actor_type/outcome must be values audit_events'
// CHECK constraints accept — otherwise the INSERT fails as a 500 the forwarder
// retries forever. The row must also be storable so the device's claim
// re-checks from it (store.FederatedRowProblem, which the store enforces too).
func invalidFederatedRow(rows []types.FederatedAuditEvent) string {
	var prev int64
	for i, e := range rows {
		switch {
		case e.Seq <= prev:
			return fmt.Sprintf("row %d: seq %d does not increase", i, e.Seq)
		case !slices.Contains([]types.ActorType{types.ActorHuman, types.ActorAgent, types.ActorSystem}, e.ActorType):
			return fmt.Sprintf("row %d: invalid actor_type %q", i, e.ActorType)
		case !slices.Contains([]string{"success", "failure", "denied"}, e.Outcome):
			return fmt.Sprintf("row %d: invalid outcome %q", i, e.Outcome)
		}
		if p := store.FederatedRowProblem(e); p != "" {
			return fmt.Sprintf("row %d: %s", i, p)
		}
		prev = e.Seq
	}
	return ""
}

// auditIngestFailure writes the device.audit.ingest failure row: which device,
// why (a closed set), how many rows it sent and where this organisation's
// record of its chain stood. Bounded like auth.failed, per device: identical
// consecutive refusals from one device fold into a streak (ingestFailures,
// keyed on device and reason) and every row written pays that device's own
// bucket — a forwarder replaying one refused batch on its backoff costs two
// rows however long it retries.
func (s *Server) auditIngestFailure(r *http.Request, d types.Device, reason string, rows int) {
	ev := s.auditEvent(nil, types.ActorSystem, deviceActor(d.ID), "device.audit.ingest", d.ID.String(), "failure",
		mustJSON(map[string]any{"reason": reason, "rows": rows, "acked_seq": d.LastSeq}))
	ev.SourceIP = r.RemoteAddr
	emit := s.ingestFailureEmitter(d.ID)
	absorbed, summary := s.ingestFailures.fold(s, d.ID.String(), reason, reason, ev, emit)
	emit(s.cfg.BaseCtx, summary)
	if absorbed {
		s.metrics.deviceIngestSuppressedInc()
		return
	}
	emit(r.Context(), &ev)
}

// handleDeviceHeartbeat is POST /api/v1/devices/{id}/heartbeat: the idle
// forwarder's keepalive. deviceAuth already touched last-seen; this answers the
// recorded cursor so the laptop can publish its lag with nothing to send.
func (s *Server) handleDeviceHeartbeat(w http.ResponseWriter, r *http.Request) {
	d, ok := deviceFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid device token")
		return
	}
	writeJSON(w, http.StatusOK, deviceAck{AckedSeq: d.LastSeq})
}
