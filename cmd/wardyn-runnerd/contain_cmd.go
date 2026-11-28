// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
)

// containExit is a contain outcome with its own process exit code (exitCode passes it through): 3 a run is not
// confirmed stopped, 4 no serve holds the lifecycle lock, 5 serve holds it but no receipt came.
type containExit struct {
	code int
	msg  string
}

func (e containExit) Error() string { return e.msg }

// The inbox and file names serve's offline manager reads (internal/runner/offline/contain.go).
const (
	containInboxDir = "contain"
	containReqExt   = ".req"
	containDoneExt  = ".done"
	containPoll     = 200 * time.Millisecond
)

// containRequestFile and containReceiptFile are the request this command writes and the receipt serve answers with.
type containRequestFile struct {
	Runs        []uuid.UUID `json:"runs,omitempty"`
	All         bool        `json:"all,omitempty"`
	RequestedAt time.Time   `json:"requested_at"`
	UID         int         `json:"uid"`
}

type containReceiptFile struct {
	Runs  map[uuid.UUID]string `json:"runs,omitempty"`
	Error string               `json:"error,omitempty"`
}

func newContainCommand() *cobra.Command {
	var runs []string
	var all bool
	var stateDir, configPath string
	wait := time.Minute
	cmd := &cobra.Command{Use: "contain (--run <run id> ... | --all)", Args: cobra.NoArgs,
		Short: "Stop runs' proxies on this host and drop their later leases, through the running serve",
		Long: "contain asks the running serve to contain runs on this host: it stops each run's proxy and drops every later " +
			"lease for it, and the organisation learns of it when the runner next connects. It never ends a run, and nothing " +
			"but ending the run clears it. With serve down it stops nothing now: serve applies it when it next starts, before " +
			"it connects."}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		req := containRequestFile{All: all, RequestedAt: time.Now().UTC(), UID: os.Getuid()}
		if all == (len(runs) > 0) {
			return errors.New("give exactly one of --run (repeatable) or --all")
		}
		for _, r := range runs {
			id, err := uuid.Parse(r)
			if err != nil {
				return fmt.Errorf("--run %q is not a run id", r)
			}
			req.Runs = append(req.Runs, id)
		}
		dir, err := containStateDir(cmd, configPath)
		if err != nil {
			return err
		}
		offlineDir := filepath.Join(dir, "offline")
		if err := checkStateOwner(offlineDir); err != nil {
			return err
		}
		id := uuid.New()
		path, err := writeContainRequest(offlineDir, id, req)
		if err != nil {
			return err
		}
		serving, err := probeServe(offlineDir)
		if err != nil {
			return err
		}
		if !serving {
			return containExit{code: 4, msg: "serve is not running: the latch is applied when serve next starts, before it connects; " +
				"to stop the proxies now, see 'Break-glass' in docs/runner-offline.md"}
		}
		rc, err := waitContainReceipt(cmd.Context(), offlineDir, id, wait)
		if errors.Is(err, errNoReceipt) {
			return containExit{code: 5, msg: fmt.Sprintf("no receipt within %s: the request stays in %s; whether the proxies stopped is unknown", wait, path)}
		}
		if err != nil {
			return err
		}
		return reportContainReceipt(cmd.OutOrStdout(), rc)
	}
	cmd.Flags().StringArrayVar(&runs, "run", nil, "a run to contain (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "contain every run on this runner")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "private runner state directory serve uses (defaults to the user configuration directory)")
	cmd.Flags().StringVar(&configPath, "config", "", "the runner's settings file, as serve reads it")
	cmd.Flags().DurationVar(&wait, "wait", wait, "how long to wait for serve's receipt")
	return cmd
}

// containStateDir is the state directory as serve resolves it: --state-dir, else runnerd.json's, else the default.
func containStateDir(cmd *cobra.Command, configPath string) (string, error) {
	cfg, err := serveConfig(cmd, configPath)
	if err != nil {
		return "", err
	}
	dir := cfg.get("state_dir")
	if dir == "" {
		if dir, err = runneridentity.DefaultDir(); err != nil {
			return "", err
		}
	}
	return filepath.Abs(dir)
}

// writeContainRequest puts req in the inbox through the offline root: written under a temporary name, synced, then
// renamed into place, so serve never reads half a request. It answers the request's path.
func writeContainRequest(offlineDir string, id uuid.UUID, req containRequestFile) (string, error) {
	root, err := os.OpenRoot(offlineDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := root.Mkdir(containInboxDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if st, err := root.Lstat(containInboxDir); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a private directory", filepath.Join(offlineDir, containInboxDir))
	}
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	name := containInboxDir + "/" + id.String() + containReqExt
	f, err := root.OpenFile(name+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(b)
	if err := errors.Join(werr, f.Chmod(0o600), f.Sync(), f.Close()); err != nil {
		return "", err
	}
	if err := root.Rename(name+".tmp", name); err != nil {
		return "", err
	}
	if d, err := root.Open(containInboxDir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return filepath.Join(offlineDir, filepath.FromSlash(name)), nil
}

var errNoReceipt = errors.New("no containment receipt")

// waitContainReceipt waits up to wait for serve's receipt of request id, then reads and removes it.
func waitContainReceipt(ctx context.Context, offlineDir string, id uuid.UUID, wait time.Duration) (containReceiptFile, error) {
	var rc containReceiptFile
	root, err := os.OpenRoot(offlineDir)
	if err != nil {
		return rc, err
	}
	defer root.Close()
	name := containInboxDir + "/" + id.String() + containDoneExt
	deadline := time.Now().Add(wait)
	for {
		f, err := root.Open(name)
		if err == nil {
			derr := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&rc)
			f.Close()
			_ = root.Remove(name)
			return rc, derr
		}
		if !errors.Is(err, os.ErrNotExist) {
			return rc, err
		}
		if !time.Now().Before(deadline) {
			return rc, errNoReceipt
		}
		select {
		case <-ctx.Done():
			return rc, ctx.Err()
		case <-time.After(containPoll):
		}
	}
}

// reportContainReceipt prints each run's outcome; any run not confirmed stopped is exit 3.
func reportContainReceipt(out io.Writer, rc containReceiptFile) error {
	if rc.Error != "" {
		return containExit{code: 3, msg: "serve did not apply the request: " + rc.Error}
	}
	ids := make([]uuid.UUID, 0, len(rc.Runs))
	for id := range rc.Runs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	open := 0
	for _, id := range ids {
		outcome := rc.Runs[id]
		fmt.Fprintf(out, "run %s: %s\n", id, outcome)
		if outcome != "stopped" && outcome != "no_proxy" {
			open++
		}
	}
	if open > 0 {
		return containExit{code: 3, msg: fmt.Sprintf("%d run(s) not confirmed stopped; serve keeps trying, and the latch holds", open)}
	}
	return nil
}
