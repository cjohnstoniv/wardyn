// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
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
// No file where toolgateCredentialWithheld names a reason: the started harness
// then holds no credential and its calls are decided as a call from no harness
// is, the strictest of all the sets. Never a refusal; the
// run.toolgate_credential.write row records delivered false with the reason.
func (s *Server) stampHarnessCredential(ctx context.Context, run types.AgentRun, sets []types.HarnessToolRules, drives []types.DriveMount, agents []string) ([]types.HarnessCredential, []runner.ManagedFile, error) {
	digests, creds, err := mintHarnessCredentials(sets, agents)
	if err != nil || len(creds) == 0 {
		return digests, nil, err
	}
	var files []runner.ManagedFile
	var named []string
	for _, agent := range agents {
		if cred, ok := creds[agent]; ok {
			named = append(named, agent)
			files = append(files, runner.ManagedFile{
				Path: runner.ComponentSecretDir + "/" + toolgateCredentialFilePrefix + agent,
				Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: []byte(cred),
			})
		}
	}
	if reason := s.toolgateCredentialWithheld(ctx, run, drives); reason != "" {
		for i, file := range files {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.toolgate_credential.write",
				run.ID.String(), "success", mustJSON(map[string]any{
					"agent": named[i], "path": file.Path, "delivered": false, "reason": reason,
				})))
		}
		return digests, nil, nil
	}
	return digests, files, nil
}

// toolgateCredentialWithheld is why the started harness's credential file
// cannot go on the spec, or "" when it can.
//
// The drive case is the drive backend's own runner target: only the
// Kubernetes driver refuses a delivered secret beside a shared drive (the
// fsGroup it needs would re-own other people's files), and a drive mounts only
// on the target its backend names. A Docker host-path share gets the file.
func (s *Server) toolgateCredentialWithheld(ctx context.Context, run types.AgentRun, drives []types.DriveMount) string {
	caps, err := s.runCapabilities(ctx, run)
	if err != nil {
		return "the runner's capabilities could not be read"
	}
	if reason, deliverable := managedFilesGap(caps, run.ConfinementClass); !deliverable {
		return reason
	}
	if slices.ContainsFunc(drives, func(d types.DriveMount) bool {
		return d.Backend.Kind() != types.DriveKindManaged && d.Backend.RunnerTarget() == "k8s"
	}) {
		return "the run mounts a shared Kubernetes drive, beside which the runner delivers no secret file"
	}
	return ""
}
