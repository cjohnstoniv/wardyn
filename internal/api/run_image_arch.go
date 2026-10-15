// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// runArch is the CPU architecture of the runner a run is bound to ("amd64" or "arm64"), or "" when the
// run is not bound to a runner (it goes to the organisation's own substrate or a remote-hosted runner,
// neither of which reports one), the runner has not reported one (a runner older than 0.9.1) or the row
// cannot be read. "" means "use the plain image ref".
func (s *Server) runArch(ctx context.Context, run types.AgentRun) string {
	st, ok := s.cfg.Store.(interface {
		GetRunner(context.Context, uuid.UUID) (types.Runner, error)
	})
	if !ok || run.RunnerID == nil {
		return ""
	}
	rn, err := st.GetRunner(ctx, *run.RunnerID)
	if err != nil {
		return ""
	}
	return rn.Arch
}

// defaultImageKey is the WARDYN_AGENT_IMAGES key the run's default image was chosen by: the agent on the
// legacy path, and for run mode the key harnessSetImage resolved ("base", a harness, or a set key). ""
// when the image came from no key.
func defaultImageKey(req createRunRequest, fact client.ImageFact) string {
	if !req.UsesRunMode() {
		return req.Agent
	}
	switch fact.Source.Kind {
	case client.ImageSourceBase:
		return "base"
	case client.ImageSourceAgent, client.ImageSourceHarnessSet:
		return fact.Source.Name
	}
	return ""
}

// archImage is the image a runner of arch uses where image (chosen by key) would otherwise be used: the
// operator's `<key>@<arch>` entry, else image itself. It only swaps the ref of a key that resolution
// already chose, so it never changes which key is used.
func archImage(image, key, arch string, variants map[string]map[string]string) string {
	if ref, ok := variants[arch][key]; ok && arch != "" && key != "" {
		return ref
	}
	return image
}
