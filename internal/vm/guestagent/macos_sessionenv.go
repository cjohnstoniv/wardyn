// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"time"
)

const (
	SessionEnvPending = "pending"
	SessionEnvSet     = "set"
	SessionEnvFailed  = "failed"

	SessionEnvNotSignedIn  = "not_signed_in"
	SessionEnvSetenvFailed = "setenv_failed"
)

// SessionEnvStatus is where a run's settings stand in the window's GUI session.
type SessionEnvStatus struct {
	State  string
	Reason string
	Via    string
	Vars   int
}

// SessionEnvPaths are the files the session settings use; the zero value is the guest's own.
type SessionEnvPaths struct{ Dir, Script, Status, Zshenv string }

func DefaultSessionEnvPaths() SessionEnvPaths { return SessionEnvPaths{} }

func SessionEnv(env, secret map[string]string, ca []string) []string { return nil }

func SessionEnvFile(p SessionEnvPaths, env []string) error { return nil }

func EnsureZshHook(p SessionEnvPaths) error { return nil }

func (s SessionEnvStatus) Line() string { return "" }

func ReadSessionEnvStatus(p SessionEnvPaths) (SessionEnvStatus, error) {
	return SessionEnvStatus{}, nil
}

func SessionEnvRetry(ctx context.Context, p SessionEnvPaths, run CmdRunner) (SessionEnvStatus, error) {
	return SessionEnvStatus{}, nil
}

func (d *Desktop) StartSessionEnv(ctx context.Context, p SessionEnvPaths, agentUID uint32, env, secret map[string]string, ca []string) <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func (d *Desktop) applySessionEnv(ctx context.Context, p SessionEnvPaths, agentUID uint32, env []string, every, wait time.Duration) SessionEnvStatus {
	return SessionEnvStatus{}
}
