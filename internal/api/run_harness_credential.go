// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// toolgateCredentialFilePrefix starts the name of the file a harness's approval
// credential is delivered as, in runner.ComponentSecretDir: the started harness's
// is toolgateCredentialFilePrefix + its name. The prefix is reserved, so no
// file_secret grant can deliver a file under it (resolveFileSecretGrants).
const toolgateCredentialFilePrefix = "wardyn-toolgate-"

// reservedToolgateCredentialFile is whether a file_secret grant's file name is
// one the approval credentials are delivered under.
func reservedToolgateCredentialFile(file string) bool {
	return strings.HasPrefix(file, toolgateCredentialFilePrefix)
}

// stampHarnessCredential mints the harness credentials and returns the digests
// for the proxy config and the started harness's plaintext as an agent-owned
// file. The file goes nowhere else: never sandbox env or SecretEnv, so neither
// the container's environment nor a process started beside the harness carries
// it. That harness's own launch in agent-run reads it into that one process's
// environment.
//
// No file when the runner cannot deliver one, or when the run mounts a shared
// drive (the Kubernetes driver refuses a delivered secret beside one): the
// started harness then holds no credential and its calls are decided as a call
// from no harness is, the strictest of all the sets. Never a refusal.
func (s *Server) stampHarnessCredential(ctx context.Context, sets []types.HarnessToolRules, agent string, drive *types.DriveMount) ([]types.HarnessCredential, []runner.ManagedFile, error) {
	digests, cred, err := mintHarnessCredentials(sets, agent)
	if err != nil || cred == "" {
		return digests, nil, err
	}
	if drive != nil && drive.Backend.Kind() != types.DriveKindManaged {
		return digests, nil, nil
	}
	if caps, cerr := s.cfg.Runner.Capabilities(ctx); cerr != nil || !caps.ManagedFiles {
		return digests, nil, nil
	}
	return digests, []runner.ManagedFile{{
		Path: runner.ComponentSecretDir + "/" + toolgateCredentialFilePrefix + agent,
		Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: []byte(cred),
	}}, nil
}
