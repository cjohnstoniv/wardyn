// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// GPUKind is the GPU a run asks for (#2958): empty for none (every run before the field).
type GPUKind string

const (
	GPUNone GPUKind = ""
	// GPUNvidia hands in every NVIDIA GPU the host's CDI specs name (nvidia.com/gpu=all).
	GPUNvidia GPUKind = "nvidia"
	// GPURender hands in the host's /dev/dri render nodes (Intel and AMD), never a card node.
	GPURender GPUKind = "render"
)

// Valid reports whether k is none or one of the two kinds.
func (k GPUKind) Valid() bool { return k == GPUNone || k == GPUNvidia || k == GPURender }
