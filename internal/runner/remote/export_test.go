// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import "github.com/cjohnstoniv/wardyn/internal/types"

// SetVMKind records ref as a VM of kind k, as a VM create's success does.
func SetVMKind(s *Substrate, ref string, k types.MachineKind) {
	s.mu.Lock()
	s.vmKinds[ref] = k
	s.mu.Unlock()
}

// VMKindOf is the machine kind the substrate holds for ref, "" when it holds none.
func VMKindOf(s *Substrate, ref string) types.MachineKind { return s.vmKind(ref) }
