// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import "net/http"

// The device hold: a walk that proves a waiting sign-in can be found after its
// browser tab was lost needs ONE sign-in that waits, while every other one keeps
// the fake's default of approving at once. POST /_control/device?hold=1 holds
// the NEXT device authorization authorization_pending; POST
// /_control/device?approve=1 approves it. The hold is per authorization, never
// the global approved flag, and approve clears a hold no sign-in has used yet
// too, so a hold a failed case left behind cannot stall a later sign-in.

// heldVerificationURI is where a held authorization sends its person: the real
// device endpoint's address, because Wardyn finds a waiting code only under the
// IAM Identity Center hosts, and the on-cluster fake has no address a browser
// reaches anyway. Nothing ever opens it: the walk approves through the control.
const heldVerificationURI = "https://device.sso.us-east-1.amazonaws.com/"

// HoldNextDevice holds the next device authorization pending until
// ApproveHeldDevices.
func (s *Server) HoldNextDevice() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdNext = true
}

// ApproveHeldDevices approves every held device authorization and drops a
// hold no sign-in has used yet.
func (s *Server) ApproveHeldDevices() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdNext = false
	for _, ss := range s.devices {
		ss.held = false
	}
}

// verificationURILocked returns ss's verification URI, holding ss when a hold
// is pending.
func (s *Server) verificationURILocked(ss *session) string {
	if !s.holdNext {
		return s.URL() + "/verify"
	}
	s.holdNext, ss.held = false, true
	return heldVerificationURI
}

// handleDeviceControl is the ON-CLUSTER form of HoldNextDevice and
// ApproveHeldDevices, on handleReauthControl's pattern.
func (s *Server) handleDeviceControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("hold") == "1" && q.Get("approve") == "":
		s.HoldNextDevice()
		writeJSON(w, http.StatusOK, map[string]any{"hold": true})
	case q.Get("approve") == "1" && q.Get("hold") == "":
		s.ApproveHeldDevices()
		writeJSON(w, http.StatusOK, map[string]any{"approved": true})
	default:
		http.Error(w, "want exactly one of hold=1 or approve=1", http.StatusBadRequest)
	}
}
