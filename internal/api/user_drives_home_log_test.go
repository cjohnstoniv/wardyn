// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The operator log line for a home name that could not be derived says which
// kind of failure it was and never the principal's claim (#2314).
func TestResolveUserDrive_HomeFailureLogCarriesNoClaim(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	d := driveFixture(func(d *types.UserDrive) {
		d.Backend, d.HomeTemplate, d.HostRoot = types.DriveBackendHostPath, types.HomeTemplateEmailLocal, "/srv/homes"
	})
	st := &driveStore{drive: d, grant: grantFixture(d.ID, nil), tier: types.CapabilitySubjectUser}
	noEmail := withOIDCGroups(operatorCtx("sub-drive-bob", "", oidc.RoleUser), []string{"eng"})
	if _, err := driveServer(st).resolveUserDrive(noEmail, 0); err == nil {
		t.Fatal("a principal with no email claim resolved an email_local home")
	}
	out := logs.String()
	if !strings.Contains(out, "a home name could not be derived") || !strings.Contains(out, "failure=no_email_claim") {
		t.Fatalf("log = %q, want the home-name line with failure=no_email_claim", out)
	}
	if strings.Contains(out, "sub-drive-bob") {
		t.Errorf("log carries the principal's claim: %q", out)
	}
}
