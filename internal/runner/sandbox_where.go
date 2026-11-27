// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// SandboxWhere is where a sandbox runs, as its driver read it at create: the node it was scheduled on and the
// RuntimeClass and handler it runs under. Each value is empty when the driver could not read it or it does not
// apply (a CC1 pod has no RuntimeClass). Only the Kubernetes driver fills it today; the server checks every value
// before it records one.
type SandboxWhere struct {
	Node           string `json:",omitempty"`
	RuntimeClass   string `json:",omitempty"`
	RuntimeHandler string `json:",omitempty"`
}
