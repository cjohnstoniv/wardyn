// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// withConfigKey sets key to value in a proxy config.
func withConfigKey(t *testing.T, b []byte, key, value string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// proxySpool is the decision_spool_dir of the config the proxy name last read on stdin, and whether the
// proxy has the spool's bind mount.
func proxySpool(t *testing.T, f *fakeDocker, name string) (dir string, mounted bool) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(f.stdinOf(name)))
	for {
		var c struct {
			Dir string `json:"decision_spool_dir"`
		}
		if err := dec.Decode(&c); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		dir = c.Dir
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.containers[name].host.Mounts {
		if m.Target == runnerSpoolTarget {
			mounted = true
		}
	}
	return dir, mounted
}

// Every proxy the driver creates, at a run's create and at a revive's replace, has decision_spool_dir in its
// config exactly when it has the spool's bind mount there. A decision_spool_dir that arrives in a received
// config never survives on its own.
func TestEveryProxyHasItsSpoolDirOnlyWithItsMount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stateDir bool
	}{{"a runner", true}, {"the organisation's own substrate", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			cfg := Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal"}
			if tc.stateDir {
				cfg.RunnerStateDir = t.TempDir()
			}
			d := newWithClient(f, cfg)
			ctx := context.Background()
			spec := testSpec()
			name := proxyContainerName(spec.RunID)
			sb, err := d.CreateSandbox(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.stateDir {
				want = runnerSpoolTarget
			}
			if dir, mounted := proxySpool(t, f, name); dir != want || mounted != tc.stateDir {
				t.Fatalf("create: decision_spool_dir %q, mounted %v; want %q and %v", dir, mounted, want, tc.stateDir)
			}
			if err := d.StopProxy(ctx, sb.Ref); err != nil {
				t.Fatal(err)
			}
			received := withConfigKey(t, renderedTestConfig(t, "fresh"), "decision_spool_dir", "/org/chosen/spool")
			if err := d.ReplaceProxy(ctx, sb.Ref, received); err != nil {
				t.Fatal(err)
			}
			if dir, mounted := proxySpool(t, f, name); dir != want || mounted != tc.stateDir {
				t.Fatalf("replace: decision_spool_dir %q, mounted %v; want %q and %v", dir, mounted, want, tc.stateDir)
			}
		})
	}
}

// stateDirEngine answers CheckStateDir's container as an engine whose view of the bind mount's source is
// the host directory (shared), an empty directory of its own (as a VM-backed engine makes one up for a
// source it cannot see), or the host directory read-only.
type stateDirEngine struct {
	*fakeDocker
	sees, writes bool
}

func (e *stateDirEngine) source(id string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.containers[id].host.Mounts {
		if m.Target == stateDirCheckTarget {
			return m.Source
		}
	}
	return ""
}

func (e *stateDirEngine) ContainerStatPath(_ context.Context, id string, opts client.ContainerStatPathOptions) (client.ContainerStatPathResult, error) {
	rel, ok := strings.CutPrefix(opts.Path, stateDirCheckTarget+"/")
	if !ok || !e.sees {
		return client.ContainerStatPathResult{}, fakeNotFound{msg: "no such path: " + opts.Path}
	}
	if _, err := os.Stat(filepath.Join(e.source(id), rel)); err != nil {
		return client.ContainerStatPathResult{}, fakeNotFound{msg: "no such path: " + opts.Path}
	}
	return client.ContainerStatPathResult{}, nil
}

func (e *stateDirEngine) CopyToContainer(_ context.Context, id string, opts client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if !e.writes {
		return client.CopyToContainerResult{}, errors.New("read-only file system")
	}
	if !e.sees {
		return client.CopyToContainerResult{}, nil // landed in the engine's own directory
	}
	tr := tar.NewReader(opts.Content)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return client.CopyToContainerResult{}, nil
		}
		if err != nil {
			return client.CopyToContainerResult{}, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return client.CopyToContainerResult{}, err
		}
		if err := os.WriteFile(filepath.Join(e.source(id), h.Name), b, 0o600); err != nil {
			return client.CopyToContainerResult{}, err
		}
	}
}

// serve refuses a state directory the engine does not see as wardyn-runnerd does, with a sentence that
// names the directory and the fix, and accepts one it shares. The check leaves no container and nothing in
// the state directory behind.
func TestCheckStateDirRefusesADirectoryTheEngineDoesNotShare(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sees, writes bool
		why          string
	}{
		{"shared", true, true, ""},
		{"an empty directory of the engine's own", false, true, "a file written there is not in the engine's view of it"},
		{"shared read-only", true, false, "the engine cannot write there"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			state := t.TempDir()
			d := newWithClient(&stateDirEngine{fakeDocker: f, sees: tc.sees, writes: tc.writes}, Config{ProxyImage: "wardyn-proxy:dev", RunnerStateDir: state, RunnerID: "runner-1"})
			err := d.CheckStateDir(context.Background())
			if tc.why == "" && err != nil {
				t.Fatalf("a shared state directory: %v", err)
			}
			if tc.why != "" && (err == nil || !strings.Contains(err.Error(), state) || !strings.Contains(err.Error(), tc.why) || !strings.Contains(err.Error(), "--state-dir")) {
				t.Fatalf("CheckStateDir = %v; want the directory %s, %q and the --state-dir fix", err, state, tc.why)
			}
			checks := 0
			for _, c := range f.containers {
				if c.cfg.Labels[labelComponent] == componentStateDirCheck {
					checks++
					if !c.removed || c.host.NetworkMode != "none" || c.cfg.Labels[labelManaged] != "true" {
						t.Fatalf("check container %s: removed %v, network %q, labels %v", c.name, c.removed, c.host.NetworkMode, c.cfg.Labels)
					}
				}
			}
			if checks != 1 {
				t.Fatalf("%d check containers, want 1", checks)
			}
			if left, _ := os.ReadDir(filepath.Join(state, "spools")); len(left) != 0 {
				t.Fatalf("the check left %v in the state directory", left)
			}
		})
	}
	f := newFakeDocker()
	if err := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"}).CheckStateDir(context.Background()); err != nil || len(f.containers) != 0 {
		t.Fatalf("no state directory: err %v, %d containers; want no check", err, len(f.containers))
	}
	if err := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RunnerStateDir: "runner"}).CheckStateDir(context.Background()); err == nil ||
		!strings.Contains(err.Error(), `"runner" is not an absolute path`) || len(f.containers) != 0 {
		t.Fatalf("a relative state directory: err %v, %d containers; want a refusal naming it", err, len(f.containers))
	}
}
