// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"cmp"
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/basesettings"
)

// setupAssistantWatch is how long endSetupAssistant looks for Setup Assistant when Desktop.SetupAssistantWatch
// is zero.
const setupAssistantWatch = 2 * time.Minute

// endSetupAssistant ends Setup Assistant in the agent's console session, so a run's first window is the
// desktop. A run's disk is thrown away (a kept machine's state is the run's own), so nothing Setup Assistant
// would record matters. It polls every SessionEnvEvery (2 s) while the agent is at the console, and stops after
// the first kill plus one poll that finds nothing to kill, or when the watch (2 min) or ctx ends. It also runs on
// a drive-open boot, where it finds nothing. pkill exits 1 when nothing matched, which Run reports as an error.
func (d *Desktop) endSetupAssistant(ctx context.Context, agentUID uint32, run CmdRunner) {
	proc := ""
	for _, k := range basesettings.Kills {
		if k.Name == "kill.setup-assistant" {
			proc = k.Process
		}
	}
	if proc == "" {
		return
	}
	every := cmp.Or(d.SessionEnvEvery, sessionEnvPollEvery)
	end := time.Now().Add(cmp.Or(d.SetupAssistantWatch, setupAssistantWatch))
	killed := false
	for ctx.Err() == nil && time.Now().Before(end) {
		if uid, err := d.Console.Console(ctx); err == nil && uid == agentUID {
			if _, err := run.Run(ctx, "/usr/bin/pkill", "-KILL", "-U", strconv.FormatUint(uint64(agentUID), 10), "-x", proc); err == nil {
				killed = true
				slog.Info("guest agent: ended Setup Assistant at run start")
			} else if killed {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
