// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Hybrid enrolment's organisation side (docs/design/0.8/PLAN.md, epic #78): the
// route mount and the admin surface — mint an enrolment token, list the
// enrolled devices, revoke one. The device-facing routes are in devices_auth.go.
package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// deviceEnrolmentTokenTTL is how long a minted token waits for its laptop's
// first boot: a weekend of slack between the admin minting it and MDM
// delivering it.
//
// ponytail: fixed; a per-mint TTL is one request field when a rollout needs it.
const deviceEnrolmentTokenTTL = 72 * time.Hour

// mintEnrolmentTokenRequest IS the SDK's body (dto_alias_test.go pins it), so
// strict decoding and the SDK cannot drift apart.
type mintEnrolmentTokenRequest = client.DeviceEnrolmentTokenRequest

// mountDeviceRoutes registers hybrid enrolment's three authentication paths,
// each its own group — the /internal groups' second-middleware shape, on the
// /api/v1 router rather than inside the human group:
//
//   - POST /devices/enrol is ANONYMOUS: a laptop's first boot holds no
//     credential yet, only the single-use token in its body, so the handler
//     carries its own per-peer limiter.
//   - /devices/{id}/audit and /heartbeat sit behind deviceAuth, which accepts a
//     wdd_ device bearer on its own {id} and nothing else, and publishes a
//     device, never a human.
//   - /admin/devices sits behind humanOrAdminAuth like every admin route:
//     minting a token creates a credential, so it is requireOperator; the
//     inventory and the revoke are the inventory-then-revoke pair /tokens
//     already puts on requireSecurityOperator.
//
// Mounted UNCONDITIONALLY: every handler type-asserts the optional
// store.DeviceStore and fails closed without it (501 here and on enrol, 401 on
// the device routes), so TestAuthzMatrix walks all of them with no special
// configuration. A mount of its own because routes() sits at the funlen cap.
func (s *Server) mountDeviceRoutes(r chi.Router) {
	r.Post("/devices/enrol", s.handleDeviceEnrol)
	r.Group(func(r chi.Router) {
		r.Use(s.deviceAuth)
		r.Post("/devices/{id}/audit", s.handleDeviceAuditIngest)
		r.Post("/devices/{id}/heartbeat", s.handleDeviceHeartbeat)
	})
	r.Group(func(r chi.Router) {
		r.Use(s.humanOrAdminAuth)
		r.With(s.requireOperator).Post("/admin/devices/enrolment-tokens", s.handleMintEnrolmentToken)
		securityOps := r.With(s.requireSecurityOperator)
		securityOps.Get("/admin/devices", s.handleListDevices)
		securityOps.Delete("/admin/devices/{id}", s.handleRevokeDevice)
	})
}

// deviceStoreOr501 is the fail-closed half of the optional capability for the
// admin routes, which answer 501 without it (the anonymous enrol route answers
// its own, content-free 501).
func (s *Server) deviceStoreOr501(w http.ResponseWriter) (store.DeviceStore, bool) {
	ds, ok := s.cfg.Store.(store.DeviceStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "device enrolment requires the Postgres store backend")
	}
	return ds, ok
}

// handleMintEnrolmentToken is POST /api/v1/admin/devices/enrolment-tokens: one
// single-use token for one named laptop, returned once — the row keeps only its
// hash — and delivered by MDM as WARDYN_ORG_ENROLMENT_TOKEN. The name becomes
// the device's inventory name, and the minter its enrolled_by.
func (s *Server) handleMintEnrolmentToken(w http.ResponseWriter, r *http.Request) {
	ds, ok := s.deviceStoreOr501(w)
	if !ok {
		return
	}
	var req mintEnrolmentTokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > apiTokenNameMaxLen || !controlCharFree(name) {
		writeError(w, http.StatusUnprocessableEntity, "name: required, at most 200 bytes, no control characters")
		return
	}
	raw := newBearer(enrolmentTokenPrefix)
	t, err := ds.MintEnrolmentToken(r.Context(), raw, types.DeviceEnrolmentToken{
		ID: uuid.New(), DeviceName: name, MintedBy: principalFromRequest(r),
		ExpiresAt: s.cfg.Now().UTC().Add(deviceEnrolmentTokenTTL),
	})
	if err != nil {
		writeServerError(w, r, "mint enrolment token", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"device.enrolment_token.create", t.ID.String(), "success",
		mustJSON(map[string]any{"device_name": t.DeviceName, "expires_at": t.ExpiresAt})))
	t.Token = raw
	writeJSON(w, http.StatusCreated, t)
}

// handleListDevices is GET /api/v1/admin/devices: every enrolled device, revoked
// ones included, newest first. No credential material — types.Device never
// serializes its hash.
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	ds, ok := s.deviceStoreOr501(w)
	if !ok {
		return
	}
	devices, err := ds.ListDevices(r.Context())
	if err != nil {
		writeServerError(w, r, "list devices", err)
		return
	}
	if devices == nil {
		devices = []types.Device{}
	}
	writeJSON(w, http.StatusOK, devices)
}

// handleRevokeDevice is DELETE /api/v1/admin/devices/{id}: the device's next
// ingest or heartbeat is 401, which its forwarder reads as revoked. Already
// revoked is 404 and writes no second row, like revokeAPIToken.
func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	ds, ok := s.deviceStoreOr501(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "device")
	if !ok {
		return
	}
	d, err := ds.RevokeDevice(r.Context(), id, s.cfg.Now().UTC())
	if notFoundIf(w, err, "device") {
		return
	}
	if err != nil {
		writeServerError(w, r, "revoke device", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"device.revoke", d.ID.String(), "success", mustJSON(map[string]any{"name": d.Name})))
	w.WriteHeader(http.StatusNoContent)
}
