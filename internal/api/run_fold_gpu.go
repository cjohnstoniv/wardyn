// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// featureGPU: "may ask for a GPU in a run" (#2958). Restricted by migration 0430 on the same terms as inbound: nobody until
// an allow names it, an `all` allow included. Asked at create, preflight and the preview when the request asks for a GPU.
const featureGPU = "gpu"

const (
	gpuUnknown       = "Unknown gpu %q. Use nvidia, render, or leave it empty."
	gpuNotGranted    = "GPU runs aren't turned on for you. Ask your admin, or start the run without a GPU."
	gpuNotAContainer = "A GPU is given only to a container run. Choose a container, or start the run without a GPU."
	gpuNotHere       = "GPU runs start only on this deployment's own executor, not on a registered runner or a runner pool. Start the run without a GPU."
	gpuNotOffered    = "This deployment offers no %s GPU for a %s run. GPU runs need Docker with CDI and the runc runtime, not the nvidia default runtime. Ask your admin, or start the run without a GPU."
)

// stepGPU admits a request's gpu ask: a known kind, the caller's gpu feature grant, a container, the deployment's own
// executor, and a substrate that hands the kind to the run's class. Every refusal comes before a row exists; the driver
// refuses again at create.
func (s *Server) stepGPU(f *runFold, _ *foldRecorder) bool {
	kind := types.GPUKind(f.req.GPU)
	if kind == types.GPUNone {
		return true
	}
	if !kind.Valid() {
		writeErrorReason(f.w, http.StatusBadRequest, reasonGPUUnknown, fmt.Sprintf(gpuUnknown, f.req.GPU))
		return false
	}
	if s.runCapabilityRefusal(f.r, capFeature, featureGPU, "runs.gpu", gpuNotGranted).write(s, f.w, f.r) {
		return false
	}
	gpuRefusal := func(msg string) bool {
		return !placementRunRefusal(&placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: "gpu"}, msg).write(s, f.w, f.r)
	}
	if types.MachineKind(f.req.MachineKind).Normalize() != "" {
		return gpuRefusal(gpuNotAContainer)
	}
	if f.place.pooled || requestPlacement(*f.req) == types.PlacementLocal {
		return gpuRefusal(gpuNotHere)
	}
	if f.mode == foldPreview {
		return true // the preview probes no runner (stepConfinement's rule)
	}
	pc, err := s.placementCapabilities(f.r.Context(), types.PlacementRemote, uuid.Nil)
	if errors.Is(err, errNoRemoteExecutor) {
		return gpuRefusal(gpuNotHere) // no executor wired: the 422 placement refusal, never a 500
	}
	if err != nil {
		return !runServerError("read runner capabilities", err).write(s, f.w, f.r)
	}
	if !slices.Contains(pc.GPU[f.enforced], kind) {
		return gpuRefusal(fmt.Sprintf(gpuNotOffered, kind, f.enforced))
	}
	return true
}
