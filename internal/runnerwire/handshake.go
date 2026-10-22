// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	ErrAuth     = errors.New("runnerwire: runner authentication refused")
	ErrRevoked  = errors.New("runnerwire: runner revoked")
	ErrGoaway   = errors.New("runnerwire: fresh session required")
	ErrNotReady = errors.New("runnerwire: session is not ready")
)

const handshakeTimeout = 15 * time.Second

// WriteControl marshals one unsequenced handshake/control payload.
func WriteControl(ctx context.Context, conn Conn, typ Type, value any) error {
	payload, err := Marshal(value)
	if err != nil {
		return err
	}
	return conn.WriteFrame(ctx, Frame{Type: typ, Payload: payload})
}

// AuthenticateRunner verifies a fresh nonce against the target runner's stored
// key. The caller must not attach any Link or disclose run data before it
// returns successfully, and must recheck admission before publishing the link.
func AuthenticateRunner(ctx context.Context, conn Conn, row types.Runner, orgURL string) (Auth, error) {
	if err := federation.CheckOrgURL(orgURL); err != nil {
		return Auth{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	var nonce [nonceLen]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Auth{}, err
	}
	hash := federation.OrgURLSHA256(orgURL)
	if row.OrgURLSHA256 != hash {
		return Auth{}, fmt.Errorf("%w: the runner was registered with another organisation URL", ErrAuth)
	}
	hello := Hello{Versions: []int{ProtocolVersion}, Nonce: nonce[:], ServerTime: time.Now().UTC(), OrgURLSHA256: hash}
	if err := WriteControl(ctx, conn, TypeHello, hello); err != nil {
		return Auth{}, err
	}
	f, err := conn.ReadFrame(ctx)
	if err != nil {
		return Auth{}, err
	}
	var auth Auth
	if f.Type != TypeAuth || Unmarshal(f.Payload, &auth) != nil || auth.Version != ProtocolVersion ||
		auth.RunnerID != row.ID.String() || !VerifyAuth(row.PublicKey, nonce[:], auth.RunnerID, hash, auth.Sig) {
		return Auth{}, fmt.Errorf("%w: the runner's proof did not verify against its registered key", ErrAuth)
	}
	if row.State == types.RunnerRevoked {
		_ = WriteControl(ctx, conn, TypeRevoked, Reason{Reason: "runner_revoked"})
		return Auth{}, ErrRevoked
	}
	// Said only to a peer that proved the key: a stranger learns nothing about which ids are registered.
	if row.ClaimExpired(time.Now()) {
		_ = WriteControl(ctx, conn, TypeGoaway, Reason{Reason: ReasonRegistrationLapsed})
		return Auth{}, fmt.Errorf("%w: the runner's claim has lapsed", ErrAuth)
	}
	return auth, nil
}

// AuthenticateOrg checks the organisation binding before signing its nonce.
// TLS and the URL's redirect refusal authenticate the server; the key proves
// possession only to the organisation this identity was registered with.
func AuthenticateOrg(ctx context.Context, conn Conn, runnerID, orgURL string, key ed25519.PrivateKey, resume *Resume) (AuthOK, error) {
	return authenticateOrg(ctx, conn, runnerID, orgURL, key, resume, nil)
}

func authenticateOrg(ctx context.Context, conn Conn, runnerID, orgURL string, key ed25519.PrivateKey, resume *Resume, clock func(Hello, time.Time) error) (AuthOK, error) {
	if _, err := StreamURL(orgURL, runnerID); err != nil {
		return AuthOK{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if len(key) != ed25519.PrivateKeySize {
		return AuthOK{}, ErrAuth
	}
	f, err := conn.ReadFrame(ctx)
	received := time.Now()
	if err != nil {
		return AuthOK{}, err
	}
	var hello Hello
	if f.Type != TypeHello || Unmarshal(f.Payload, &hello) != nil || len(hello.Nonce) != nonceLen ||
		!slices.Contains(hello.Versions, ProtocolVersion) || hello.OrgURLSHA256 != federation.OrgURLSHA256(orgURL) {
		return AuthOK{}, ErrAuth
	}
	if clock != nil {
		if hello.ServerTime.IsZero() {
			return AuthOK{}, ErrAuth
		}
		if err := clock(hello, received); err != nil {
			return AuthOK{}, err
		}
	}
	auth := Auth{RunnerID: runnerID, Version: ProtocolVersion, Resume: resume,
		Sig: SignAuth(key, hello.Nonce, runnerID, hello.OrgURLSHA256)}
	if err := WriteControl(ctx, conn, TypeAuth, auth); err != nil {
		return AuthOK{}, err
	}
	f, err = conn.ReadFrame(ctx)
	if err != nil {
		return AuthOK{}, err
	}
	switch f.Type {
	case TypeRevoked:
		return AuthOK{}, ErrRevoked
	case TypeGoaway:
		return AuthOK{}, goaway(f.Payload)
	case TypeAuthOK:
		return checkAuthOK(f.Payload, resume)
	default:
		return AuthOK{}, fmt.Errorf("%w: expected AUTH_OK", ErrBadFrame)
	}
}

// ReasonRegistrationLapsed is the GOAWAY reason for a runner whose registration lapsed before it was
// claimed, or changed while it connected.
const ReasonRegistrationLapsed = "registration_lapsed"

// goaway returns ErrGoaway carrying the org's stated reason, when it gave one.
func goaway(payload []byte) error {
	var r Reason
	if Unmarshal(payload, &r) != nil || r.Reason == "" {
		return ErrGoaway
	}
	if say, ok := goawaySentences[r.Reason]; ok {
		return fmt.Errorf("%w: %s (%s)", ErrGoaway, say, r.Reason)
	}
	return fmt.Errorf("%w: %s", ErrGoaway, r.Reason)
}

// goawaySentences say what the org's GOAWAY reasons mean to the person reading the runner's log.
var goawaySentences = map[string]string{
	ReasonRegistrationLapsed: "this runner's unclaimed registration lapsed, or its registration changed (register it again with a new token)",
	"runner_unclaimed":       "this runner is waiting for its owner to claim it (run `wardyn runner claim`)",
	"runner_owner_inactive":  "this runner's owner is deactivated, so the organisation will not admit it",
}

func checkAuthOK(payload []byte, resume *Resume) (AuthOK, error) {
	var ok AuthOK
	if err := Unmarshal(payload, &ok); err != nil {
		return ok, err
	}
	id, err := uuid.Parse(ok.SessionID)
	if err != nil || id.String() != ok.SessionID || ok.PingInterval != PingInterval ||
		(ok.Resumed && (resume == nil || ok.SessionID != resume.SessionID)) || (!ok.Resumed && ok.LastSeq != 0) {
		return AuthOK{}, fmt.Errorf("%w: invalid AUTH_OK", ErrBadFrame)
	}
	return ok, nil
}
