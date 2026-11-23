// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Organisation sites: an admin's label for a place over organization pools and admin drives, with one default
// pool. Placement reads them (run_placement_site.go); these routes write them.
//
//	GET /org-sites, GET /org-sites/{id}                     member: a site names a live pool only when the
//	                                                        caller's catalogue shows it
//	GET /org-sites/drives                                   security (admin or security_admin), as GET /drives
//	POST /org-sites, PUT and DELETE /org-sites/{id},        admin (SUPER)
//	PUT and DELETE /runner-pools/{id}/site,
//	PUT and DELETE /drives/{id}/site
//
// While features.org_sites is off, every write that adds is refused 403 org_sites_off; removing a pool from its
// site, clearing a drive's home and deleting a site are allowed, since they only narrow.
func (s *Server) registerOrgSiteRoutes(r, operatorOnly, securityOps chi.Router) {
	r.Get("/org-sites", s.handleListOrgSites)
	securityOps.Get("/org-sites/drives", s.handleListOrgSiteDrives)
	r.Get("/org-sites/{id}", s.handleGetOrgSite)
	operatorOnly.Post("/org-sites", s.handleCreateOrgSite)
	operatorOnly.Put("/org-sites/{id}", s.handleUpdateOrgSite)
	operatorOnly.Delete("/org-sites/{id}", s.handleDeleteOrgSite)
	operatorOnly.Put("/runner-pools/{id}/site", s.handleAssignPoolSite)
	operatorOnly.Delete("/runner-pools/{id}/site", s.handleClearPoolSite)
	operatorOnly.Put("/drives/{id}/site", s.handleSetDriveSite)
	operatorOnly.Delete("/drives/{id}/site", s.handleClearDriveSite)
}

// The audit actions of the site routes. A row names the site and carries the storage note's label and
// whether a link is set (storage_link_set), never the link itself.
const (
	auditOrgSiteCreate      = "org_site.create"
	auditOrgSiteUpdate      = "org_site.update"
	auditOrgSiteDelete      = "org_site.delete"
	auditOrgSitePoolAssign  = "org_site.pool.assign"
	auditOrgSitePoolRemove  = "org_site.pool.unassign"
	auditOrgSiteDriveSet    = "org_site.drive.set"
	auditOrgSiteDriveRemove = "org_site.drive.clear"
)

const orgSitesOffMsg = "Sites are off: an admin turns them on with features.org_sites.enabled in the site config. " +
	"Taking a pool or a drive off a site, and deleting a site, still work."

var errNoOrgSiteStore = errors.New("this store has no site storage")

// createOrgSiteRequest is POST /org-sites. A new site has no pools, so it has no default pool yet.
type createOrgSiteRequest struct {
	Name         string `json:"name"`
	StorageLabel string `json:"storage_label,omitempty"`
	StorageURL   string `json:"storage_url,omitempty"`
}

// updateOrgSiteRequest is PUT /org-sites/{id}: the whole site as it should read, at the revision the caller read.
type updateOrgSiteRequest struct {
	Revision      int64      `json:"revision"`
	Name          string     `json:"name"`
	StorageLabel  string     `json:"storage_label,omitempty"`
	StorageURL    string     `json:"storage_url,omitempty"`
	DefaultPoolID *uuid.UUID `json:"default_pool_id,omitempty"`
}

// orgSiteRef is the body of PUT /runner-pools/{id}/site and PUT /drives/{id}/site.
type orgSiteRef struct {
	SiteID uuid.UUID `json:"site_id"`
}

// poolSiteAnswer is PUT /runner-pools/{id}/site's answer: the pool's site and its revision, which a move raises.
type poolSiteAnswer struct {
	PoolID   uuid.UUID `json:"pool_id"`
	SiteID   uuid.UUID `json:"site_id"`
	Revision int64     `json:"revision"`
}

// driveSiteAnswer is PUT /drives/{id}/site's answer.
type driveSiteAnswer struct {
	DriveID uuid.UUID `json:"drive_id"`
	SiteID  uuid.UUID `json:"site_id"`
}

func (s *Server) orgSiteStore(w http.ResponseWriter, r *http.Request) (store.OrgSiteStore, bool) {
	ss, ok := s.cfg.Store.(store.OrgSiteStore)
	if !ok {
		writeServerError(w, r, "sites", errNoOrgSiteStore)
	}
	return ss, ok
}

// refuseSitesOff answers a write that adds while sites are off. It reads the site config itself, and an
// unreadable one refuses too.
func (s *Server) refuseSitesOff(w http.ResponseWriter, r *http.Request) bool {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "read site config", err)
		return true
	}
	if !siteFeatureOn(sc, featureOrgSites) {
		writeErrorReason(w, http.StatusForbidden, reasonOrgSitesOff, orgSitesOffMsg)
		return true
	}
	return false
}

func writeOrgSiteNotFound(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusNotFound, reasonOrgSiteNotFound, "site not found")
}

// readOrgSite reads one site; with live set, a deleted site is the 404 an unknown id gets. ok=false: the
// answer is written.
func (s *Server) readOrgSite(w http.ResponseWriter, r *http.Request, ss store.OrgSiteStore, id uuid.UUID, live bool) (types.OrgSite, bool) {
	site, err := ss.GetOrgSite(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && live && site.State != types.OrgSiteActive) {
		writeOrgSiteNotFound(w)
		return types.OrgSite{}, false
	}
	if err != nil {
		writeServerError(w, r, "read site", err)
		return types.OrgSite{}, false
	}
	return site, true
}

// visibleOrgSites keeps, on each site, only the live pools the caller's pool catalogue shows (every live pool for
// a security admin or an admin), so a site never names a live pool the pool routes answer as unknown. A deleted
// pool still at a site stays listed, so an admin can take it off: no one can use it. ok=false: the answer is
// written.
func (s *Server) visibleOrgSites(w http.ResponseWriter, r *http.Request, sites []types.OrgSite) ([]types.OrgSite, bool) {
	if sites == nil {
		sites = []types.OrgSite{}
	}
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return sites, true
	}
	rows, ok := s.poolCatalogue(w, r, ps, false)
	if !ok {
		return nil, false
	}
	shown := map[uuid.UUID]bool{}
	for _, row := range rows {
		shown[row.Pool.ID] = true
	}
	for _, site := range sites {
		for _, id := range site.PoolIDs {
			if shown[id] {
				continue
			}
			p, err := ps.GetRunnerPool(r.Context(), id)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				writeServerError(w, r, "read runner pool", err)
				return nil, false
			}
			shown[id] = err == nil && p.State == types.RunnerPoolDeleted
		}
	}
	for i := range sites {
		sites[i].PoolIDs = slices.DeleteFunc(slices.Clone(sites[i].PoolIDs), func(id uuid.UUID) bool { return !shown[id] })
		if sites[i].PoolIDs == nil {
			sites[i].PoolIDs = []uuid.UUID{}
		}
		if d := sites[i].DefaultPoolID; d != nil && !shown[*d] {
			sites[i].DefaultPoolID = nil
		}
	}
	return sites, true
}

func (s *Server) handleListOrgSites(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	sites, err := ss.ListOrgSites(r.Context())
	if err != nil {
		writeServerError(w, r, "list sites", err)
		return
	}
	if sites, ok = s.visibleOrgSites(w, r, sites); ok {
		writeJSON(w, http.StatusOK, sites)
	}
}

func (s *Server) handleGetOrgSite(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "site")
	if !ok {
		return
	}
	site, ok := s.readOrgSite(w, r, ss, id, false)
	if !ok {
		return
	}
	if sites, ok := s.visibleOrgSites(w, r, []types.OrgSite{site}); ok {
		writeJSON(w, http.StatusOK, sites[0])
	}
}

// handleListOrgSiteDrives lists the admin drives homed at a live site, for the security tier that lists drives.
func (s *Server) handleListOrgSiteDrives(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	drives, err := ss.ListOrgSiteDrives(r.Context())
	if err != nil {
		writeServerError(w, r, "list drive sites", err)
		return
	}
	if drives == nil {
		drives = []types.OrgSiteDrive{}
	}
	writeJSON(w, http.StatusOK, drives)
}

func (s *Server) handleCreateOrgSite(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	var req createOrgSiteRequest
	if !decodeStrict(w, r, &req) || s.refuseSitesOff(w, r) {
		return
	}
	site := types.OrgSite{ID: uuid.New(), Name: req.Name, StorageLabel: req.StorageLabel, StorageURL: req.StorageURL}
	if err := site.Validate(); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonOrgSiteInvalid, err.Error())
		return
	}
	out, err := ss.CreateOrgSite(r.Context(), site)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeErrorReason(w, http.StatusConflict, reasonOrgSiteNameTaken, "a site with that name already exists")
		return
	case err != nil:
		writeServerError(w, r, "create site", err)
		return
	}
	s.auditOrgSite(r, auditOrgSiteCreate, out.ID, orgSiteAuditData(out))
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleUpdateOrgSite(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "site")
	if !ok {
		return
	}
	var req updateOrgSiteRequest
	if !decodeStrict(w, r, &req) || s.refuseSitesOff(w, r) {
		return
	}
	if err := (types.OrgSite{Name: req.Name, StorageLabel: req.StorageLabel, StorageURL: req.StorageURL}).Validate(); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonOrgSiteInvalid, err.Error())
		return
	}
	cur, ok := s.readOrgSite(w, r, ss, id, true)
	if !ok {
		return
	}
	// The audit row is built from cur, so another revision is refused here; the store checks it again under
	// the site's row lock.
	if req.Revision != cur.Revision {
		writeOrgSiteStale(w)
		return
	}
	if req.DefaultPoolID != nil && !slices.Contains(cur.PoolIDs, *req.DefaultPoolID) {
		writeOrgSiteDefaultPool(w)
		return
	}
	out, err := ss.UpdateOrgSite(r.Context(), id, req.Revision, req.Name, req.StorageLabel, req.StorageURL, req.DefaultPoolID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeOrgSiteNotFound(w)
		return
	case errors.Is(err, store.ErrOrgSiteStale):
		writeOrgSiteStale(w)
		return
	case errors.Is(err, store.ErrConflict):
		writeErrorReason(w, http.StatusConflict, reasonOrgSiteNameTaken, "a site with that name already exists")
		return
	case errors.Is(err, store.ErrOrgSiteDefaultPool):
		writeOrgSiteDefaultPool(w)
		return
	case err != nil:
		writeServerError(w, r, "update site", err)
		return
	}
	if out.Revision != cur.Revision {
		s.auditOrgSite(r, auditOrgSiteUpdate, out.ID, orgSiteUpdateAuditData(cur, out))
	}
	writeJSON(w, http.StatusOK, out)
}

func writeOrgSiteStale(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusConflict, reasonOrgSiteStale, "the site changed since it was read: read it again and retry")
}

func writeOrgSiteDefaultPool(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusUnprocessableEntity, reasonOrgSiteDefaultPool, "a site's default pool is one of the live pools at that site")
}

func (s *Server) handleDeleteOrgSite(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "site")
	if !ok {
		return
	}
	out, err := ss.DeleteOrgSite(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeOrgSiteNotFound(w)
		return
	case errors.Is(err, store.ErrOrgSiteInUse):
		writeErrorReason(w, http.StatusConflict, reasonOrgSiteInUse, "take every pool and drive off the site first")
		return
	case err != nil:
		writeServerError(w, r, "delete site", err)
		return
	}
	s.auditOrgSite(r, auditOrgSiteDelete, out.ID, map[string]any{"site_id": out.ID, "site_name": out.Name, "revision": out.Revision})
	w.WriteHeader(http.StatusNoContent)
}

// orgSitePool reads the organization pool {id} for a site change; a deleted pool is accepted only when live is
// false. ok=false: the answer is written.
func (s *Server) orgSitePool(w http.ResponseWriter, r *http.Request, live bool) (store.RunnerPoolStore, types.RunnerPool, bool) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return nil, types.RunnerPool{}, false
	}
	id, ok := parseIDParam(w, r, "id", "runner pool")
	if !ok {
		return nil, types.RunnerPool{}, false
	}
	p, err := ps.GetRunnerPool(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && live && p.State == types.RunnerPoolDeleted) {
		writePoolNotFound(w)
		return nil, types.RunnerPool{}, false
	}
	if err != nil {
		writeServerError(w, r, "read runner pool", err)
		return nil, types.RunnerPool{}, false
	}
	if p.Scope != types.RunnerPoolOrganization {
		writeOrgSitePoolScope(w, "only an organization pool can be at a site; a person's own pool cannot")
		return nil, types.RunnerPool{}, false
	}
	return ps, p, true
}

func writeOrgSitePoolScope(w http.ResponseWriter, msg string) {
	writeErrorReason(w, http.StatusUnprocessableEntity, reasonOrgSitePoolScope, msg)
}

// handleAssignPoolSite puts an organization pool at a site, moving it from any other. A move raises the pool's
// revision, so a run admitted against the pool before it is refused as stale, never recorded at the old site.
func (s *Server) handleAssignPoolSite(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	_, pool, ok := s.orgSitePool(w, r, true)
	if !ok {
		return
	}
	var req orgSiteRef
	if !decodeStrict(w, r, &req) || s.refuseSitesOff(w, r) {
		return
	}
	site, ok := s.readOrgSite(w, r, ss, req.SiteID, true)
	if !ok {
		return
	}
	before, at, err := ss.PoolSite(r.Context(), pool.ID)
	if err != nil {
		writeServerError(w, r, "read the pool's site", err)
		return
	}
	from, rev, err := ss.AssignPoolSite(r.Context(), pool.ID, site.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonOrgSiteNotFound, "the site or the pool was deleted meanwhile")
		return
	case errors.Is(err, store.ErrRunnerPoolScope):
		writeOrgSitePoolScope(w, "only an organization pool can be at a site; a person's own pool cannot")
		return
	case err != nil:
		writeServerError(w, r, "assign pool site", err)
		return
	}
	if from == nil || *from != site.ID {
		data := map[string]any{"pool_id": pool.ID, "pool_name": pool.Name, "site_id": site.ID, "site_name": site.Name, "revision": rev}
		if from != nil {
			data["site_before"] = *from
			data["default_pool_cleared"] = at && before.ID == *from && before.DefaultPoolID != nil && *before.DefaultPoolID == pool.ID
		}
		s.auditOrgSite(r, auditOrgSitePoolAssign, site.ID, data)
	}
	writeJSON(w, http.StatusOK, poolSiteAnswer{PoolID: pool.ID, SiteID: site.ID, Revision: rev})
}

// handleClearPoolSite takes an organization pool, live or deleted, off its site, and clears the site's
// default when it named the pool. A pool at no site is left as it is, with no audit row.
func (s *Server) handleClearPoolSite(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	_, pool, ok := s.orgSitePool(w, r, false)
	if !ok {
		return
	}
	before, at, err := ss.PoolSite(r.Context(), pool.ID)
	if err != nil {
		writeServerError(w, r, "read the pool's site", err)
		return
	}
	from, rev, err := ss.ClearPoolSite(r.Context(), pool.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writePoolNotFound(w)
		return
	case errors.Is(err, store.ErrRunnerPoolScope):
		writeOrgSitePoolScope(w, "only an organization pool can be at a site; a person's own pool cannot")
		return
	case err != nil:
		writeServerError(w, r, "clear pool site", err)
		return
	}
	if from != nil {
		data := map[string]any{"pool_id": pool.ID, "pool_name": pool.Name, "site_id": *from, "revision": rev}
		if at && before.ID == *from {
			data["site_name"] = before.Name
			data["default_pool_cleared"] = before.DefaultPoolID != nil && *before.DefaultPoolID == pool.ID
		}
		s.auditOrgSite(r, auditOrgSitePoolRemove, *from, data)
	}
	w.WriteHeader(http.StatusNoContent)
}

// siteDrive reads the drive {id} for a home-site change. ok=false: the answer is written.
func (s *Server) siteDrive(w http.ResponseWriter, r *http.Request) (types.UserDrive, bool) {
	id, ok := parseIDParam(w, r, "id", "user drive")
	if !ok {
		return types.UserDrive{}, false
	}
	d, err := s.cfg.Store.GetUserDrive(r.Context(), id)
	if notFoundIf(w, err, "user drive", reasonUserDriveNotFound) {
		return types.UserDrive{}, false
	}
	if err != nil {
		writeServerError(w, r, "read user drive", err)
		return types.UserDrive{}, false
	}
	return d, true
}

const memberDriveSiteMsg = "Only an admin drive has a home site; a member drive is made where its run goes"

// handleSetDriveSite homes an admin drive at a site, replacing any home it had.
func (s *Server) handleSetDriveSite(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	d, ok := s.siteDrive(w, r)
	if !ok {
		return
	}
	var req orgSiteRef
	if !decodeStrict(w, r, &req) || s.refuseSitesOff(w, r) {
		return
	}
	if d.OwnerSubject != "" {
		writeOrgSitePoolScope(w, memberDriveSiteMsg)
		return
	}
	site, ok := s.readOrgSite(w, r, ss, req.SiteID, true)
	if !ok {
		return
	}
	from, err := ss.SetDriveSite(r.Context(), d.ID, site.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonOrgSiteNotFound, "the site or the drive was deleted meanwhile")
		return
	case errors.Is(err, store.ErrOrgSiteMemberDrive):
		writeOrgSitePoolScope(w, memberDriveSiteMsg)
		return
	case err != nil:
		writeServerError(w, r, "set drive site", err)
		return
	}
	if from == nil || *from != site.ID {
		data := map[string]any{"drive_id": d.ID, "drive_name": d.Name, "site_id": site.ID, "site_name": site.Name}
		if from != nil {
			data["site_before"] = *from
		}
		s.auditOrgSite(r, auditOrgSiteDriveSet, site.ID, data)
	}
	writeJSON(w, http.StatusOK, driveSiteAnswer{DriveID: d.ID, SiteID: site.ID})
}

// handleClearDriveSite takes a drive's home site away. A drive with no home is left as it is, with no audit row.
func (s *Server) handleClearDriveSite(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.orgSiteStore(w, r)
	if !ok {
		return
	}
	d, ok := s.siteDrive(w, r)
	if !ok {
		return
	}
	from, err := ss.ClearDriveSite(r.Context(), d.ID)
	if err != nil {
		writeServerError(w, r, "clear drive site", err)
		return
	}
	if from != nil {
		data := map[string]any{"drive_id": d.ID, "drive_name": d.Name, "site_id": *from}
		if site, err := ss.GetOrgSite(r.Context(), *from); err == nil {
			data["site_name"] = site.Name
		}
		s.auditOrgSite(r, auditOrgSiteDriveRemove, *from, data)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditOrgSite(r *http.Request, action string, site uuid.UUID, data map[string]any) {
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), action, site.String(), "success", mustJSON(data)))
}

// orgSiteAuditData is a site as its audit rows carry it: the storage link's presence, never the link.
func orgSiteAuditData(site types.OrgSite) map[string]any {
	data := map[string]any{"site_id": site.ID, "site_name": site.Name, "storage_link_set": site.StorageURL != "", "revision": site.Revision}
	if site.StorageLabel != "" {
		data["storage_label"] = site.StorageLabel
	}
	if site.DefaultPoolID != nil {
		data["default_pool_id"] = *site.DefaultPoolID
	}
	return data
}

// orgSiteUpdateAuditData is an update's row: the site as it now reads, and what it was for each part that changed.
func orgSiteUpdateAuditData(cur, out types.OrgSite) map[string]any {
	data := orgSiteAuditData(out)
	if cur.Name != out.Name {
		data["site_name_before"] = cur.Name
	}
	if cur.StorageLabel != out.StorageLabel {
		data["storage_label_before"] = cur.StorageLabel
	}
	if cur.StorageURL != out.StorageURL {
		data["storage_link_changed"] = true
	}
	if !uuidPtrEqual(cur.DefaultPoolID, out.DefaultPoolID) {
		data["default_pool_id_before"] = cur.DefaultPoolID
	}
	return data
}

func uuidPtrEqual(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
