// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/go-chi/chi/v5"

// mountDriveAndRunAdminRoutes is the tail of adminRoutes (routes.go), registered at the same point and in the same
// order: the SUPER-only routes for the legacy Docker drives, standing runs and identity re-binding.
func (s *Server) mountDriveAndRunAdminRoutes(operatorOnly chi.Router) {
	// Releases the person's Docker volumes that existed at the 0.10.2 upgrade (legacy_docker_release.go). SUPER: it
	// answers for every person's drives, and an administrator states by calling it that every older server is stopped.
	operatorOnly.Post("/admin/drives/legacy-docker/release", s.handleReleaseLegacyDockerDrives)
	// Standing runs (run_revive.go). SUPER: a restart replaces the proxy of
	// runs the caller does not own, under each owner's own ceiling, and the
	// listing reads the whole fleet.
	operatorOnly.Get("/admin/runs/proxy-window", s.handleAdminProxyWindow)
	operatorOnly.Post("/admin/runs/restart", s.handleAdminRestartRuns)
	// Entra app re-registration remedy (identity_unbind.go). SUPER: it re-opens a
	// person's identity row to a different sign-in principal.
	operatorOnly.Post("/admin/identities/{id}/unbind", s.handleUnbindIdentity)
}
