// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const runnerSpoolTarget = "/var/lib/wardyn/decision-spool"

// proxyImageUser is the proxy image's own user and group (distroless nonroot).
const proxyImageUser = 65532

// processUID and chownSpool are this process's uid and the spool hand-over, swapped in tests, which
// cannot be root.
var (
	processUID = os.Getuid
	chownSpool = chownSpoolTree
)

// chownSpoolTree gives the spool directory and every entry in it (the spool holds no subdirectory) to
// uid:gid, never following a link and never a file that has a second link.
func chownSpoolTree(dir string, uid, gid int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, name := range append(names, ".") {
		// A file with another link is left as it is (spoolowner.Keep's rule): a file outside the spool
		// is never handed over through a link to it.
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && !info.IsDir() && st.Nlink != 1 {
			continue
		}
		if err := root.Lchown(name, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) runnerSpoolConfig(ctx context.Context, id uuid.UUID, b []byte, cfg *container.Config, host *container.HostConfig) ([]byte, error) {
	if d.cfg.RunnerStateDir == "" {
		return b, nil
	}
	if !filepath.IsAbs(d.cfg.RunnerStateDir) || id == uuid.Nil {
		return nil, errors.New("runner spool requires an absolute state directory and run id")
	}
	dir := filepath.Join(d.cfg.RunnerStateDir, "spools", id.String())
	spool, err := runnerspool.Open(dir, id)
	if err != nil {
		return nil, fmt.Errorf("runner spool: %w", err)
	}
	if err := spool.Close(); err != nil {
		return nil, err
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("runner proxy config is empty")
	}
	data["decision_spool_dir"], err = json.Marshal(runnerSpoolTarget)
	if err != nil {
		return nil, err
	}
	b, err = json.Marshal(data)
	if err != nil {
		return nil, err
	}
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, err
	}
	// Rootless Docker maps its container root to the daemon owner. A rootful daemon needs the caller's
	// uid/gid to create files runnerd can read at 0600, unless runnerd is root: root reads them anyway,
	// so the proxy keeps its image's non-root user and the spool is handed to that user.
	switch {
	case slices.Contains(info.Info.SecurityOptions, "name=rootless"):
		cfg.User = "0:0"
	case processUID() == 0:
		if err := chownSpool(dir, proxyImageUser, proxyImageUser); err != nil {
			return nil, fmt.Errorf("runner spool: hand to the proxy's user: %w", err)
		}
		cfg.User = fmt.Sprintf("%d:%d", proxyImageUser, proxyImageUser)
	default:
		cfg.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeBind, Source: dir, Target: runnerSpoolTarget, BindOptions: &mount.BindOptions{NonRecursive: true}})
	return b, nil
}
