// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"slices"
	"time"
)

// Stage names where a runner's last attempt to connect, or to start, stopped. It is a closed list the
// organisation checks, and the only thing about a failure that leaves the runner: never an error's text,
// which may hold a proxy credential or a URL. A stage is a label the runner asserts, and nothing is
// admitted by it.
type Stage string

const (
	StageDNS          Stage = "dns"          // the organisation's host name did not resolve
	StageProxy        Stage = "proxy"        // the proxy named by HTTPS_PROXY refused or did not answer
	StageNetwork      Stage = "network"      // the connection to the organisation did not open or timed out
	StageTLS          Stage = "tls"          // the organisation's certificate was not trusted
	StageUpgrade      Stage = "upgrade"      // the WebSocket upgrade was answered with something else
	StageAuth         Stage = "auth"         // the organisation refused the runner's proof
	StageRegistration Stage = "registration" // the organisation will not admit this registration (lapsed, unclaimed, owner deactivated, revoked)
	StageSession      Stage = "session"      // the session ended after it was admitted
	StageEngine       Stage = "engine"       // startup: the container engine did not answer
	StageImage        Stage = "image"        // startup: the proxy image is neither on the engine nor pullable
	StageRelay        Stage = "relay"        // the run relay is not reachable from the engine
	StageCA           Stage = "ca"           // startup: a CA file was refused
)

// Stages is every Stage, the closed list.
var Stages = []Stage{StageDNS, StageProxy, StageNetwork, StageTLS, StageUpgrade, StageAuth, StageRegistration, StageSession,
	StageEngine, StageImage, StageRelay, StageCA}

// Valid reports whether s is in the closed list.
func (s Stage) Valid() bool {
	for _, k := range Stages {
		if k == s {
			return true
		}
	}
	return false
}

// Failure is the last stage that failed and when. The caps event carries it on each connect so the
// organisation can say why a runner was out of reach.
type Failure struct {
	Stage Stage     `json:"stage"`
	At    time.Time `json:"at"`
	// Ongoing says the failure is still going on (the run relay, which is checked while the runner is
	// connected); every other stage is over by the time a connect reports it.
	Ongoing bool `json:"ongoing,omitempty"`
}

// stageError carries the Stage a failed connection attempt stopped at beside the original error, whose
// text and chain it keeps.
type stageError struct {
	stage Stage
	err   error
}

func (e *stageError) Error() string { return e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// upgradeError is a dial the organisation, or a proxy before it, answered with other than 101.
type upgradeError struct {
	err    error
	status int
}

func (e *upgradeError) Error() string { return e.err.Error() }
func (e *upgradeError) Unwrap() error { return e.err }

// StageOf is the Stage err stopped at: the one Connect recorded, else session. It reads error types
// only, never their text.
func StageOf(err error) Stage {
	var se *stageError
	if errors.As(err, &se) {
		return se.stage
	}
	return StageSession
}

// withStage records s on err, keeping its text and chain.
func withStage(s Stage, err error) error {
	if err == nil {
		return nil
	}
	return &stageError{stage: s, err: err}
}

// dialStage classifies a failed dial by the type of its error.
func dialStage(err error) Stage {
	var verify *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	var refused *proxyRefusedError
	var upgrade *upgradeError
	var op *net.OpError
	var dns *net.DNSError
	switch {
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &invalid), errors.As(err, &record):
		return StageTLS
	case errors.As(err, &refused):
		return StageProxy
	case errors.As(err, &op) && op.Op == "proxyconnect":
		return StageProxy
	case errors.As(err, &dns):
		return StageDNS
	case errors.As(err, &upgrade):
		return StageUpgrade
	}
	return StageNetwork
}

// authStage classifies a handshake that ended without a session: auth for a key the organisation did
// not accept, else as serveStage does, so a routine GOAWAY (resume_gap) is not a registration refusal.
func authStage(err error) Stage {
	if errors.Is(err, ErrAuth) {
		return StageAuth
	}
	return serveStage(err)
}

// registrationReasons are the GOAWAY reasons that mean the organisation will not admit this registration.
// An unclaimed runner is sent its AUTH_OK first and then the GOAWAY, so the refusal arrives mid-session.
var registrationReasons = []string{ReasonRegistrationLapsed, "runner_unclaimed", "runner_owner_inactive", "runner_revoked"}

// serveStage is the Stage a session that ended with err stopped at: registration when the organisation
// sent a GOAWAY for one of registrationReasons or revoked the runner, else session. It reads the typed
// reason code only.
func serveStage(err error) Stage {
	var g *goawayReasonError
	switch {
	case errors.Is(err, ErrRevoked):
		return StageRegistration
	case errors.As(err, &g) && slices.Contains(registrationReasons, g.reason):
		return StageRegistration
	}
	return StageSession
}
