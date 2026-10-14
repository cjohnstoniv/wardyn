// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// RemoteRunnerPeoplePools is a remote-hosted runner's "Available to people's pools" policy: who may
// add it to their own user pools. Off by default; checked when a person adds it and again at launch.
type RemoteRunnerPeoplePools string

const (
	PeoplePoolsOff      RemoteRunnerPeoplePools = "off"
	PeoplePoolsEveryone RemoteRunnerPeoplePools = "everyone"
	PeoplePoolsListed   RemoteRunnerPeoplePools = "listed"
)

// Valid reports whether p is one of the three.
func (p RemoteRunnerPeoplePools) Valid() bool {
	return p == PeoplePoolsOff || p == PeoplePoolsEveryone || p == PeoplePoolsListed
}

// RemoteRunner is a remote-hosted runner: a Docker host or Kubernetes cluster this server is set up
// with, keyed by the name of the substrate it is configured as. A row whose substrate this server does
// not run stays, and is never a candidate.
type RemoteRunner struct {
	ID   string     `json:"id"`
	Kind RunnerKind `json:"kind"`
	Name string     `json:"name"`
	// PeoplePools is who may add it to their own user pools; PeopleSubjects lists them when it is
	// listed. A listed policy whose people all left lists nobody and admits nobody.
	PeoplePools    RemoteRunnerPeoplePools `json:"people_pools"`
	PeopleSubjects []RunnerPoolSubject     `json:"people_subjects,omitempty"`
	// Limits is what an administrator declared (proposed once at boot from what the server measured);
	// nil while any of them is unset, and then the runner takes no run.
	Limits *RunnerLimits `json:"limits"`
	// Revision starts at 1 and rises on every change to the name or the people policy.
	Revision  int64     `json:"revision"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RemoteRunnerNameMax is the longest display name of a remote-hosted runner, in characters.
const RemoteRunnerNameMax = 64

// ValidateRemoteRunnerName accepts a printable, trimmed, single-line name.
func ValidateRemoteRunnerName(name string) error {
	if name == "" || name != strings.TrimSpace(name) || len([]rune(name)) > RemoteRunnerNameMax {
		return errors.New("name a runner in 1 to 64 characters, without leading or trailing spaces")
	}
	if strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return errors.New("a runner name is one printable line")
	}
	return nil
}

// RemoteRunnerDefaultName is the name a configured remote-hosted runner gets when its row is made.
func RemoteRunnerDefaultName(id string) string {
	switch id {
	case "docker":
		return "Docker host"
	case "k8s":
		return "Kubernetes cluster"
	}
	return id
}

// RemoteRunnerPoolRef names one live pool a remote-hosted runner is in.
type RemoteRunnerPoolRef struct {
	ID    uuid.UUID       `json:"id"`
	Name  string          `json:"name"`
	Scope RunnerPoolScope `json:"scope"`
	// Owner is a user pool's owner, set on the operators' reads only.
	Owner string `json:"owner,omitempty"`
}
