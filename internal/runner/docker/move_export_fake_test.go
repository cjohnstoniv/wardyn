// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// exportDocker is a daemon holding one stopped container with the root /a. Once goneAfter archive calls have
// been answered, the container is gone: inspects and archive calls answer not-found, as a removed
// container's do.
type exportDocker struct {
	*fakeDocker
	mu        sync.Mutex
	copies    int
	goneAfter int
	state     *container.State
}

func (e *exportDocker) gone() bool { return e.goneAfter > 0 && e.copies >= e.goneAfter }

func (e *exportDocker) ContainerInspect(_ context.Context, ref string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gone() {
		return client.ContainerInspectResult{}, fakeNotFound{msg: "no such container: " + ref}
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{ID: "c1", State: e.state, Config: &container.Config{}}}, nil
}

func (e *exportDocker) CopyFromContainer(_ context.Context, ref string, o client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gone() {
		return client.CopyFromContainerResult{}, fakeNotFound{msg: "no such container: " + ref}
	}
	e.copies++
	if o.SourcePath != "/a" {
		return client.CopyFromContainerResult{}, fakeNotFound{msg: "no such path: " + o.SourcePath}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "a/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "a/f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	return client.CopyFromContainerResult{Content: io.NopCloser(&buf)}, nil
}

// readExport reads an export's stream to its end.
func readExport(t *testing.T, d *Driver, set runner.CarriedSet) (movepack.Manifest, error) {
	t.Helper()
	out, err := d.ExportState(context.Background(), "agent-x", set)
	if err != nil {
		return movepack.Manifest{}, err
	}
	defer out.Stream.Close()
	r, err := movepack.NewReader(out.Stream, movepack.Options{Set: set})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for {
		_, content, err := r.Next()
		if err == io.EOF {
			return r.Manifest(), nil
		}
		if err != nil {
			return movepack.Manifest{}, err
		}
		if content != nil {
			_, _ = io.Copy(io.Discard, content)
		}
	}
}

// TestMoveExport_VanishedContainerFailsTheExport: a container removed during an export fails it, whether
// the removal shows as a missing root or happens after the last root; it never ends as a short stream with a
// manifest of its own. A root the container still there lacks is carried as nothing.
func TestMoveExport_VanishedContainerFailsTheExport(t *testing.T) {
	stopped := &container.State{Status: "exited"}
	rows := []struct {
		name      string
		roots     []string
		goneAfter int
		wantErr   bool
	}{
		{"gone before a later root", []string{"/a", "/b"}, 1, true},
		{"gone after the last root", []string{"/a"}, 1, true},
		{"a root the container lacks", []string{"/a", "/b"}, 0, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := &exportDocker{fakeDocker: newFakeDocker(), goneAfter: row.goneAfter, state: stopped}
			d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
			m, err := readExport(t, d, runner.CarriedSet{Roots: row.roots})
			if !row.wantErr {
				if err != nil || m.Entries != 2 {
					t.Fatalf("export = %+v, %v; want the two entries of /a", m, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "changed during the export") {
				t.Fatalf("export of a container removed under it = %+v, %v; want it failed", m, err)
			}
		})
	}
}
