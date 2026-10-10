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

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const runnerSpoolTarget = "/var/lib/wardyn/decision-spool"

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
	// Rootless Docker maps its container root to the daemon owner; a rootful
	// daemon needs the caller's uid/gid to create files runnerd can read at0600.
	if slices.Contains(info.Info.SecurityOptions, "name=rootless") {
		cfg.User = "0:0"
	} else {
		cfg.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeBind, Source: dir, Target: runnerSpoolTarget, BindOptions: &mount.BindOptions{NonRecursive: true}})
	return b, nil
}
