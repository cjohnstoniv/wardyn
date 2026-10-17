// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/google/uuid"
)

// SetAsidePrevious moves the run state an earlier registration left in stateDir (its offline state,
// spools and output) into stateDir/previous, once the proxy of every run that state still holds is
// stopped. The organisation binds each of those runs to the earlier runner, so no action for them
// can ever reach this one, and New refuses state another runner wrote. State this runner wrote, or
// none, is left as it is. The caller holds the serve lock.
func SetAsidePrevious(ctx context.Context, stateDir string, runnerID uuid.UUID, sub substrate.Substrate, expectedRef func(uuid.UUID) string) error {
	stopper, ok := sub.(runner.ProxyStopper)
	if !ok {
		return runner.ErrEndUnsupported
	}
	path := filepath.Join(stateDir, "offline", "state.json")
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > stateLimit {
		return errors.New("unsafe runner lifecycle state")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var old localState
	if err := json.Unmarshal(b, &old); err != nil || old.RunnerID == runnerID || old.RunnerID == uuid.Nil {
		// This runner's own state, or one New refuses with its own reason.
		return nil
	}
	var failed []error
	for id, r := range old.Runs {
		if !r.Retired && r.ProxyStoppedAt == nil && r.Ref != "" && r.Ref == expectedRef(id) {
			if err := stopper.StopProxy(ctx, r.Ref); err != nil {
				failed = append(failed, err)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the run state in %s belongs to an earlier registration of this computer (runner %s), and %d of its runs' proxies could not be stopped (%w): make the container engine or cluster reachable, then restart wardyn-runnerd, which stops them and moves that state into %s",
			stateDir, old.RunnerID, len(failed), failed[0], filepath.Join(stateDir, "previous"))
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "previous"), 0700); err != nil {
		return err
	}
	dest, err := os.MkdirTemp(filepath.Join(stateDir, "previous"), old.RunnerID.String()+"-")
	if err != nil {
		return err
	}
	// The offline state goes last: until it moves, the next start finds it and tries again.
	for _, name := range []string{"spools", "output", "offline"} {
		if err := os.Rename(filepath.Join(stateDir, name), filepath.Join(dest, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("move the earlier registration's %s into %s: %w", name, dest, err)
		}
	}
	slog.WarnContext(ctx, "runner stopped an earlier registration's run proxies and moved its run state aside", "runner", old.RunnerID, "moved_to", dest)
	return nil
}
