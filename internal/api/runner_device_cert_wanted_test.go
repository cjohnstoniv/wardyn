// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The hub's device_cert answer is whether an MDM device CA is stored, as the macOS placement gate reads it. A site
// config that cannot be read, or no store at all, is an error, which AUTH_OK reads as no and the recheck as no answer.
func TestRunnerDeviceCertWanted_FollowsTheStoredMDMCA(t *testing.T) {
	for _, c := range []struct {
		name    string
		st      *mdmStore
		want    bool
		wantErr bool
	}{
		{"no runner settings", &mdmStore{}, false, false},
		{"no CA", &mdmStore{cfg: types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}}, false, false},
		{"CA set", &mdmStore{cfg: types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true,
			MDMDeviceCAPEM: "-----BEGIN CERTIFICATE-----", MDMDeviceCRLURL: "https://ca.example/ca.crl"}}}, true, false},
		{"unreadable", &mdmStore{err: errors.New("site config unreadable")}, false, true},
	} {
		s := &Server{cfg: Config{Store: c.st}}
		got, err := s.runnerDeviceCertWanted(t.Context())
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("%s: got %v, %v; want %v, error %v", c.name, got, err, c.want, c.wantErr)
		}
	}
	if got, err := (&Server{}).runnerDeviceCertWanted(t.Context()); got || err == nil {
		t.Errorf("no store: got %v, %v; want false and an error", got, err)
	}
}
