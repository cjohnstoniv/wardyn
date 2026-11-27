// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// RunExtraContainer is one extra container a run keeps, as create admitted it.
type RunExtraContainer struct {
	Name           string                 `json:"name"`
	Image          string                 `json:"image"` // the admitted pin when admission pinned one
	Command        []string               `json:"command,omitempty"`
	Ports          []int                  `json:"ports,omitempty"`
	CPUMillis      int                    `json:"cpu_millis"`
	MemoryMiB      int                    `json:"memory_mib"`
	ImageAdmission *ImageAdmissionVerdict `json:"image_admission,omitempty"`
}
