// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Windows licence pools, their holders and seats, and the registered Windows images. A pool is the
// organisation's own count of seats, which Wardyn enforces; it is not a licence, and the audit is the
// record. While features.windows_vm is off, reads are answered, adds are refused and removals allowed.

// The audit actions of the licence pool and image writes.
const (
	auditWindowsPoolCreate     = "windows_licence.pool.create"
	auditWindowsPoolUpdate     = "windows_licence.pool.update"
	auditWindowsPoolDelete     = "windows_licence.pool.delete"
	auditWindowsHolderAdd      = "windows_licence.holder.add"
	auditWindowsHolderRemove   = "windows_licence.holder.remove"
	auditWindowsImageRegister  = "windows_image.register"
	auditWindowsImageUpdate    = "windows_image.update"
	auditWindowsImageRetire    = "windows_image.retire"
	windowsLicenceNotFoundMsg  = "No active Windows licence pool, holder or image has that id."
	windowsLicenceNameTakenMsg = "That name, ref or holder is already taken in an active Windows licence pool or image."
)

// registerWindowsLicenceRoutes mounts the pool, holder and seat routes on the SUPER tier, beside the
// organisation's runner pools, the image routes on the security tier, beside /base-images, and the
// member's own image list on r.
func (s *Server) registerWindowsLicenceRoutes(r, operatorOnly, securityOps chi.Router) {
	operatorOnly.Get("/windows-licence-pools", s.handleListWindowsPools)
	operatorOnly.Post("/windows-licence-pools", s.handleCreateWindowsPool)
	operatorOnly.Get("/windows-licence-pools/{id}", s.handleGetWindowsPool)
	operatorOnly.Put("/windows-licence-pools/{id}", s.handleUpdateWindowsPool)
	operatorOnly.Delete("/windows-licence-pools/{id}", s.handleDeleteWindowsPool)
	operatorOnly.Get("/windows-licence-pools/{id}/holders", s.handleListWindowsHolders)
	operatorOnly.Post("/windows-licence-pools/{id}/holders", s.handleAddWindowsHolder)
	operatorOnly.Delete("/windows-licence-pools/{id}/holders/{subject}", s.handleRemoveWindowsHolder)
	operatorOnly.Get("/windows-licence-pools/{id}/seats", s.handleListWindowsSeats)
	securityOps.Get("/windows-images", s.handleListWindowsImages)
	securityOps.Post("/windows-images", s.handleRegisterWindowsImage)
	securityOps.Get("/windows-images/{id}", s.handleGetWindowsImage)
	securityOps.Put("/windows-images/{id}", s.handleUpdateWindowsImage)
	securityOps.Delete("/windows-images/{id}", s.handleRetireWindowsImage)
	r.Get("/me/windows-images", s.handleListMyWindowsImages)
}

type windowsPoolRequest struct {
	Name     *string `json:"name,omitempty"`
	Seats    *int    `json:"seats,omitempty"`
	Revision int64   `json:"revision,omitempty"`
}

type windowsHolderRequest struct {
	Subject string `json:"subject"`
}

type windowsImageRequest struct {
	Name          *string    `json:"name,omitempty"`
	Ref           string     `json:"ref,omitempty"`
	LicencePoolID *uuid.UUID `json:"licence_pool_id,omitempty"`
	Revision      int64      `json:"revision,omitempty"`
}

// windowsLiveSeat is a live seat: the run that holds it, the holder and since when.
type windowsLiveSeat struct {
	RunID  uuid.UUID `json:"run_id"`
	Holder string    `json:"holder"`
	Since  time.Time `json:"since"`
}

func (s *Server) windowsLicenceStore(w http.ResponseWriter) (store.WindowsLicenceStore, bool) {
	ws, ok := s.cfg.Store.(store.WindowsLicenceStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonWindowsLicencesUnavailable, windowsLicencesMissingMsg)
	}
	return ws, ok
}

// writeWindowsLicenceErr answers a licence write's error: a fence refusal naming name, not found, a taken name,
// a malformed body the store refused, or a server error.
func writeWindowsLicenceErr(w http.ResponseWriter, r *http.Request, op, name string, err error) {
	if rf := windowsLicenceRefusal(err, name); rf != nil {
		writeErrorReason(w, rf.status, rf.body.Reason, rf.body.Error)
		return
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonWindowsLicenceNotFound, windowsLicenceNotFoundMsg)
	case errors.Is(err, store.ErrConflict):
		writeErrorReason(w, http.StatusConflict, reasonWindowsLicenceConflict, windowsLicenceNameTakenMsg)
	default:
		writeServerError(w, r, op, err)
	}
}

func writeWindowsLicenceInvalid(w http.ResponseWriter, err error) {
	writeErrorReason(w, http.StatusBadRequest, reasonWindowsLicenceInvalid, capitalFirst(err.Error())+".")
}

func capitalFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Server) auditWindowsLicence(r *http.Request, action, target string, data map[string]any) {
	at, principal := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, at, principal, action, target, "success", mustJSON(data)))
}

// refuseWindowsAddWhileOff answers 403 windows_vms_off for an add while the switch is off.
func (s *Server) refuseWindowsAddWhileOff(w http.ResponseWriter, r *http.Request) bool {
	return s.windowsVMsOffRefusal(r.Context()).write(s, w, r)
}

func (s *Server) handleListWindowsPools(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	pools, err := ws.ListWindowsLicencePools(r.Context())
	if err != nil {
		writeServerError(w, r, "list Windows licence pools", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": nonNil(pools)})
}

func (s *Server) handleCreateWindowsPool(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok || s.refuseWindowsAddWhileOff(w, r) {
		return
	}
	var req windowsPoolRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	_, principal := actorFromRequest(r)
	p := types.WindowsLicencePool{ID: uuid.New(), CreatedBy: principal}
	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.Seats != nil {
		p.Seats = *req.Seats
	}
	if err := p.Validate(); err != nil {
		writeWindowsLicenceInvalid(w, err)
		return
	}
	p, err := ws.CreateWindowsLicencePool(r.Context(), p)
	if err != nil {
		writeWindowsLicenceErr(w, r, "create Windows licence pool", p.Name, err)
		return
	}
	s.auditWindowsLicence(r, auditWindowsPoolCreate, p.ID.String(), map[string]any{"name": p.Name, "seats": p.Seats, "revision": p.Revision})
	writeJSON(w, http.StatusCreated, p)
}

// windowsPathID parses {id}; the store answers an id with no active row as not found.
func windowsPathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	return parseIDParam(w, r, "id", "Windows licence pool or image")
}

func (s *Server) handleGetWindowsPool(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	p, err := ws.GetWindowsLicencePool(r.Context(), id)
	if err != nil {
		writeWindowsLicenceErr(w, r, "get Windows licence pool", "", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleUpdateWindowsPool(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	var req windowsPoolRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Revision < 1 || (req.Name == nil && req.Seats == nil) {
		writeErrorReason(w, http.StatusBadRequest, reasonWindowsLicenceInvalid, "Send the pool's revision, and a name or seats to change.")
		return
	}
	if req.Seats != nil {
		if err := types.ValidateWindowsLicenceSeats(*req.Seats); err != nil {
			writeWindowsLicenceInvalid(w, err)
			return
		}
	}
	p, err := ws.UpdateWindowsLicencePool(r.Context(), id, req.Revision, req.Name, req.Seats)
	if err != nil {
		writeWindowsLicenceErr(w, r, "update Windows licence pool", "", err)
		return
	}
	s.auditWindowsLicence(r, auditWindowsPoolUpdate, p.ID.String(), map[string]any{"name": p.Name, "seats": p.Seats, "revision": p.Revision})
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteWindowsPool(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	name := s.windowsPoolName(r.Context(), ws, id)
	p, err := ws.DeleteWindowsLicencePool(r.Context(), id)
	if err != nil {
		writeWindowsLicenceErr(w, r, "delete Windows licence pool", name, err)
		return
	}
	s.auditWindowsLicence(r, auditWindowsPoolDelete, p.ID.String(), map[string]any{"name": p.Name, "revision": p.Revision})
	writeJSON(w, http.StatusOK, p)
}

// windowsPoolName names a pool in a refusal, "" when it cannot be read (the refusal then says "the pool").
func (s *Server) windowsPoolName(ctx context.Context, ws store.WindowsLicenceStore, id uuid.UUID) string {
	if p, err := ws.GetWindowsLicencePool(ctx, id); err == nil {
		return "The Windows licence pool " + p.Name
	}
	return ""
}

func (s *Server) handleListWindowsHolders(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	if _, err := ws.GetWindowsLicencePool(r.Context(), id); err != nil {
		writeWindowsLicenceErr(w, r, "get Windows licence pool", "", err)
		return
	}
	holders, err := ws.ListWindowsLicenceHolders(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "list Windows licence holders", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holders": nonNil(holders)})
}

func (s *Server) handleAddWindowsHolder(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok || s.refuseWindowsAddWhileOff(w, r) {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	var req windowsHolderRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if types.NormalizeWindowsLicenceSubject(req.Subject) == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonWindowsLicenceInvalid, "Name the person who holds the seat in subject.")
		return
	}
	_, principal := actorFromRequest(r)
	name := ""
	if p, err := ws.GetWindowsLicencePool(r.Context(), id); err == nil {
		name = p.Name
	}
	h, err := ws.AddWindowsLicenceHolder(r.Context(), id, req.Subject, principal)
	if err != nil {
		writeWindowsLicenceErr(w, r, "add Windows licence holder", name, err)
		return
	}
	s.auditWindowsLicence(r, auditWindowsHolderAdd, id.String(), map[string]any{"subject": h.Subject})
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) handleRemoveWindowsHolder(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	subject := chi.URLParam(r, "subject")
	if u, err := url.PathUnescape(subject); r.URL.RawPath != "" && err == nil {
		subject = u // a client that percent-encoded the subject, as principalParam reads it
	}
	h, err := ws.RemoveWindowsLicenceHolder(r.Context(), id, subject)
	if err != nil {
		writeWindowsLicenceErr(w, r, "remove Windows licence holder", "The seat "+types.NormalizeWindowsLicenceSubject(subject)+" holds", err)
		return
	}
	s.auditWindowsLicence(r, auditWindowsHolderRemove, id.String(), map[string]any{"subject": h.Subject})
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleListWindowsSeats(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	if _, err := ws.GetWindowsLicencePool(r.Context(), id); err != nil {
		writeWindowsLicenceErr(w, r, "get Windows licence pool", "", err)
		return
	}
	leases, err := ws.ListWindowsLicenceLeases(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "list Windows licence seats", err)
		return
	}
	holders, err := ws.ListWindowsLicenceHolders(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "list Windows licence holders", err)
		return
	}
	subjects := map[uuid.UUID]string{}
	for _, h := range holders {
		subjects[h.ID] = h.Subject
	}
	seats := make([]windowsLiveSeat, 0, len(leases))
	for _, l := range leases {
		seats = append(seats, windowsLiveSeat{RunID: l.RunID, Holder: subjects[l.HolderID], Since: l.TakenAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"seats": seats})
}

func (s *Server) handleListWindowsImages(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	imgs, err := ws.ListWindowsImages(r.Context())
	if err != nil {
		writeServerError(w, r, "list Windows images", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": nonNil(imgs)})
}

func (s *Server) handleRegisterWindowsImage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok || s.refuseWindowsAddWhileOff(w, r) {
		return
	}
	var req windowsImageRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	_, principal := actorFromRequest(r)
	img := types.WindowsImage{ID: uuid.New(), Ref: req.Ref, CreatedBy: principal}
	if req.Name != nil {
		img.Name = *req.Name
	}
	if req.LicencePoolID != nil {
		img.LicencePoolID = *req.LicencePoolID
	}
	if err := img.Validate(); err != nil {
		writeWindowsLicenceInvalid(w, err)
		return
	}
	img, err := ws.RegisterWindowsImage(r.Context(), img)
	if err != nil {
		writeWindowsLicenceErr(w, r, "register Windows image", img.Name, err)
		return
	}
	s.auditWindowsLicence(r, auditWindowsImageRegister, img.ID.String(), windowsImageAudit(img))
	writeJSON(w, http.StatusCreated, img)
}

// windowsImageAudit is an image write's audit data: the name, the digest-pinned ref and the pool.
func windowsImageAudit(img types.WindowsImage) map[string]any {
	return map[string]any{"name": img.Name, "image": img.Ref, "licence_pool_id": img.LicencePoolID.String(), "revision": img.Revision}
}

func (s *Server) handleGetWindowsImage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	img, err := ws.GetWindowsImage(r.Context(), id)
	if err != nil {
		writeWindowsLicenceErr(w, r, "get Windows image", "", err)
		return
	}
	writeJSON(w, http.StatusOK, img)
}

func (s *Server) handleUpdateWindowsImage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	var req windowsImageRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Revision < 1 || req.Ref != "" || (req.Name == nil && req.LicencePoolID == nil) {
		writeErrorReason(w, http.StatusBadRequest, reasonWindowsLicenceInvalid,
			"Send the image's revision, and a name or licence_pool_id to change. A ref is never changed: register a new image.")
		return
	}
	name := s.windowsImageName(r.Context(), ws, id)
	img, err := ws.UpdateWindowsImage(r.Context(), id, req.Revision, req.Name, req.LicencePoolID)
	if err != nil {
		writeWindowsLicenceErr(w, r, "update Windows image", name, err)
		return
	}
	s.auditWindowsLicence(r, auditWindowsImageUpdate, img.ID.String(), windowsImageAudit(img))
	writeJSON(w, http.StatusOK, img)
}

func (s *Server) handleRetireWindowsImage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	id, ok := windowsPathID(w, r)
	if !ok {
		return
	}
	name := s.windowsImageName(r.Context(), ws, id)
	img, err := ws.RetireWindowsImage(r.Context(), id)
	if err != nil {
		writeWindowsLicenceErr(w, r, "retire Windows image", name, err)
		return
	}
	s.auditWindowsLicence(r, auditWindowsImageRetire, img.ID.String(), windowsImageAudit(img))
	writeJSON(w, http.StatusOK, img)
}

// windowsImageName names an image in a refusal, "" when it cannot be read.
func (s *Server) windowsImageName(ctx context.Context, ws store.WindowsLicenceStore, id uuid.UUID) string {
	if img, err := ws.GetWindowsImage(ctx, id); err == nil {
		return "The Windows image " + img.Name
	}
	return ""
}

// handleListMyWindowsImages is GET /me/windows-images: the active images in pools the caller holds a seat in,
// the ones a Windows VM run of theirs may start from.
func (s *Server) handleListMyWindowsImages(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.windowsLicenceStore(w)
	if !ok {
		return
	}
	ctx := r.Context()
	_, principal := actorFromRequest(r)
	me := types.NormalizeWindowsLicenceSubject(principal)
	imgs, err := ws.ListWindowsImages(ctx)
	if err != nil {
		writeServerError(w, r, "list Windows images", err)
		return
	}
	held := map[uuid.UUID]bool{}
	out := []types.WindowsImage{}
	for _, img := range imgs {
		if img.State != types.WindowsImageActive || me == "" {
			continue
		}
		in, seen := held[img.LicencePoolID]
		if !seen {
			holders, err := ws.ListWindowsLicenceHolders(ctx, img.LicencePoolID)
			if err != nil {
				writeServerError(w, r, "list Windows licence holders", err)
				return
			}
			for _, h := range holders {
				in = in || h.Subject == me
			}
			held[img.LicencePoolID] = in
		}
		if in {
			out = append(out, img)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": out})
}

// nonNil answers an empty list as [] rather than null.
func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}
