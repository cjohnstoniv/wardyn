// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

var (
	macGUIEnvPollEvery = 2 * time.Second
	macGUIEnvWait      = 10 * time.Minute
	macGUIEnvCacheTTL  = 3 * time.Second
)

const macWindowSettingsPendingMsg = "This run's desktop settings are still being applied; the window opens when they are."

var macGUIEnvArgv = []string{"/bin/cat", guestagent.MacSessionEnvStatusPath}

func (s *Server) macGUIEnvAwait(ctx context.Context, run types.AgentRun) {}
