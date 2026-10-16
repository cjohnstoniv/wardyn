// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// resolveDockerHost is the engine address wardyn-runnerd uses, chosen the way the docker CLI chooses
// its engine: DOCKER_HOST (which the CLI reads before DOCKER_CONTEXT), else DOCKER_CONTEXT, else the
// context the CLI last selected, else the default. The Docker SDK reads DOCKER_HOST alone, so a selected context would otherwise send the
// runner to a different engine than `docker` reaches. Only a context that is a local unix socket can
// be followed; any other (tcp, ssh, TLS) is refused with the DOCKER_HOST to set, never guessed at.
// "" is the SDK's default socket.
func resolveDockerHost(getenv func(string) string, home string) (string, error) {
	configDir := getenv("DOCKER_CONFIG")
	if configDir == "" {
		configDir = filepath.Join(home, ".docker")
	}
	if getenv("DOCKER_HOST") != "" {
		return getenv("DOCKER_HOST"), nil
	}
	name := getenv("DOCKER_CONTEXT")
	if name == "" {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if b, err := os.ReadFile(filepath.Join(configDir, "config.json")); err == nil {
			_ = json.Unmarshal(b, &cfg)
		}
		name = cfg.CurrentContext
	}
	if name == "" || name == "default" {
		return getenv("DOCKER_HOST"), nil
	}
	sum := sha256.Sum256([]byte(name))
	metaPath := filepath.Join(configDir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
	raw, err := os.ReadFile(metaPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("docker context %q is selected but not found under %s: run `docker context use default`, or set DOCKER_HOST to the engine's socket", name, configDir)
	}
	if err != nil {
		return "", fmt.Errorf("docker context %q: %w", name, err)
	}
	var meta struct {
		Endpoints map[string]struct{ Host string }
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("docker context %q: %s is not readable: %w", name, metaPath, err)
	}
	host := meta.Endpoints["docker"].Host
	switch {
	case host == "":
		return getenv("DOCKER_HOST"), nil
	case strings.HasPrefix(host, "unix://"):
		return host, nil
	}
	return "", fmt.Errorf("docker context %q points at %s: wardyn-runnerd follows only a context that is a local unix socket (no tcp, ssh or TLS). Set DOCKER_HOST to the socket of the engine the runner should use, or run `docker context use default`", name, host)
}

// untransportedDockerEnv are the variables the Docker SDK reads that setup does not carry into the
// service, which gets DOCKER_HOST alone. Set at setup, they would send setup's engine check to an engine
// the running service never reaches.
var untransportedDockerEnv = []string{"DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", "DOCKER_API_VERSION"}

// checkUntransportedDockerEnv refuses setup when one of untransportedDockerEnv is set.
func checkUntransportedDockerEnv(getenv func(string) string) error {
	for _, v := range untransportedDockerEnv {
		if getenv(v) != "" {
			return fmt.Errorf("%s is set, but the service setup installs receives only DOCKER_HOST, so it would reach a different engine than the one setup checks: unset %s for setup (an engine that needs TLS or a pinned API version is not supported; use a local socket)", v, v)
		}
	}
	return nil
}
