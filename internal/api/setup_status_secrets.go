// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "context"

// setupSecretsSnapshot lists the operator's user secrets (names only, reserved
// excluded) and derives the presence map and the SetupSecrets row from them.
// A pure move out of handleSetupStatus (golangci funlen, 150 lines): the
// caller writes the 500 on error exactly as before.
func (s *Server) setupSecretsSnapshot(ctx context.Context) ([]string, map[string]bool, SetupSecrets, error) {
	secretNames := []string{}
	if s.cfg.Secrets != nil {
		names, err := s.listUserSecretNames(ctx)
		if err != nil {
			return nil, nil, SetupSecrets{}, err
		}
		secretNames = names
	}
	present := make(map[string]bool, len(secretNames))
	for _, n := range secretNames {
		present[n] = true
	}
	sec := SetupSecrets{
		Present:   secretNames,
		GitHubApp: present[secretGitHubAppID] && present[secretGitHubAppKey],
	}
	return secretNames, present, sec, nil
}
