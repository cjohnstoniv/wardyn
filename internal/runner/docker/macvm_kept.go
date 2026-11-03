// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A kept machine is one person's macOS guest disk, kept on their own Mac between runs. Each session of
// it is a new run (SandboxSpec.KeptMachine). Wardyn keeps what the person made in it and never what
// Wardyn delivered: a secret that would live inside the guest is refused here, and what else a run
// writes is scrubbed by the guest agent.

// KeptConfig is MacVMConfig's part for kept machines. wardyn-runnerd fills it.
type KeptConfig struct {
	// LimitBytes is the disk this Mac's kept machines may take, each counted at its worst case; zero
	// keeps no machines on this Mac.
	LimitBytes int64
	// Tools brings a kept machine's tools up to this Mac's before it boots; nil leaves them as they are.
	Tools KeptToolsUpdater
}

// KeptToolsUpdater updates the guest agent and pinned harness CLIs on a kept machine's disk, in its
// folder, when the machine's tools are behind this Mac's.
type KeptToolsUpdater interface {
	UpdateKeptTools(ctx context.Context, keptDir string) error
}

// keptMCPSecretEnv is the one secret env a kept machine's session may carry: the MCP broker routes. Each
// URL holds its run's token, which is dead once that run's proxy is gone, and the scrub strips them.
const keptMCPSecretEnv = "WARDYN_MCP_SERVERS_JSON"

// refuseKeptSecrets refuses a kept machine's session that would put a Wardyn secret inside the guest,
// where the kept disk would keep it: any secret env but the MCP routes, and any file delivered in
// runner.ComponentSecretDir. The organisation refuses these before dispatch; this is the Mac's own
// check. It names keys and paths only, never a value.
func refuseKeptSecrets(spec runner.SandboxSpec) error {
	if spec.KeptMachine == nil {
		return nil
	}
	var held []string
	for k := range spec.SecretEnv {
		if k != keptMCPSecretEnv {
			held = append(held, "secret env "+k)
		}
	}
	slices.Sort(held)
	for _, f := range spec.ManagedFiles {
		if p := path.Clean(f.Path); p == runner.ComponentSecretDir || strings.HasPrefix(p, runner.ComponentSecretDir+"/") {
			held = append(held, "secret file "+p)
		}
	}
	if len(held) == 0 {
		return nil
	}
	return fmt.Errorf("docker: a kept machine takes no secret that lives inside it, and this run carries %s", strings.Join(held, ", "))
}
