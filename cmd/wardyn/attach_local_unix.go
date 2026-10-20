// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import "github.com/cjohnstoniv/wardyn/internal/runneridentity"

// localAttachOS: the local transport exists on this OS.
const localAttachOS = true

func defaultLocalAttachDir() (string, error) { return runneridentity.PointerDir() }

// localSocketFor is the socket of runnerID's pointer file in dir, when that file, its directory and the
// socket are this user's and the runner belongs to the organisation at orgURL; else "" and why not.
func localSocketFor(dir, runnerID, orgURL string) (string, string) {
	p, err := runneridentity.ReadPointer(dir, runnerID)
	if err != nil {
		return "", "no usable pointer file for runner " + runnerID + ": " + err.Error()
	}
	if !p.ForOrg(orgURL) {
		return "", "runner " + runnerID + " belongs to another organisation"
	}
	return p.Socket, ""
}
