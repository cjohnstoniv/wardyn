// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

// MemberDriveSeedRequest is POST /api/v1/me/drives/{id}/seed: fill the caller's own drive from one repository of one
// of their workspaces, through their own exec run.
type MemberDriveSeedRequest struct {
	WorkspaceID string `json:"workspace_id"`
	Repo        string `json:"repo"`
	Ref         string `json:"ref,omitempty"`
	// Into is a sub-folder of the drive to fill; "" fills the drive's top folder, which must then be empty.
	Into string `json:"into,omitempty"`
}

// MemberDriveSeedResult is the 202 answer: the run that fills the drive.
type MemberDriveSeedResult struct {
	RunID  string `json:"run_id"`
	Target string `json:"target"`
}
