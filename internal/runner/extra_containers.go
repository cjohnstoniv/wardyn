// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "errors"

// ExtraContainer is one container a sandbox runs beside the agent (SandboxSpec.Containers): in the agent's
// network, with its work folder and drives. The substrate sets everything else: the security context, an
// environment of the proxy's variables only, and no secret. CPUMillis and MemoryMiB are carved out of
// SandboxSpec.Resources. A substrate that does not advertise Capabilities.ExtraContainers refuses a spec
// carrying any (ErrExtraContainersUnsupported); the control plane never sends it one.
type ExtraContainer struct {
	Name      string   `json:"name"`
	Image     string   `json:"image"`             // pinned by digest
	Command   []string `json:"command,omitempty"` // replaces the image's entrypoint; empty keeps it
	Ports     []int    `json:"ports,omitempty"`   // declared, never exposed
	CPUMillis int64    `json:"cpu_millis"`
	MemoryMiB int64    `json:"memory_mib"`
}

// ErrExtraContainersUnsupported is a substrate's refusal of a spec whose Containers it cannot run. The
// control plane sends such a spec only where Capabilities.ExtraContainers is true; this is the
// substrate's own fail-closed backstop.
var ErrExtraContainersUnsupported = errors.New("runner: this sandbox cannot run extra containers")
