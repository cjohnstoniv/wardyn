// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var (
	_ runner.StateExporter       = (*Driver)(nil)
	_ runner.StateImporter       = (*Driver)(nil)
	_ runner.MoveImportPresenter = (*Driver)(nil)
)

// MoveImportReadsPresent marks the Docker driver as a move destination: the container an import creates
// is "created", which Status reads RunStarting, so the runner's observer reports the moved run present.
func (*Driver) MoveImportReadsPresent() {}

// maxImportLinkBytes bounds the symlinks an import holds back to write after every other entry.
const maxImportLinkBytes = 64 << 20

// bootedMarker is the file the image's boot reads to take its revive branch, so an imported run's seed
// does not run again (agent-run-lib.sh).
const bootedMarker = agentImageHome + "/.wardyn/booted"

// ExportState reads set's roots out of ref, a stopped container, through the daemon's archive API. The
// daemon archives a root that is a symlink as the link itself, and never follows a link inside one; the
// stream refuses the first and carries the rest as their text. A root the container lacks is carried as
// nothing, but only once the container is shown still there: the daemon answers a missing container and a
// missing path alike. The driver keeps no spool, so the export answers SpoolDrained.
func (d *Driver) ExportState(ctx context.Context, ref string, set runner.CarriedSet) (runner.StateExport, error) {
	id, err := d.exportable(ctx, ref)
	if err != nil {
		return runner.StateExport{}, err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(d.exportTo(ctx, ref, id, set, pw)) }()
	return runner.StateExport{Stream: pr, SpoolDrained: true}, nil
}

// exportable inspects ref and answers its container id when it is a stopped container whose files a move
// carries.
func (d *Driver) exportable(ctx context.Context, ref string) (string, error) {
	insp, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("docker: export %s: %w", ref, err)
	}
	c := insp.Container
	switch {
	case c.Config != nil && c.Config.Labels[labelMachineKind] != "":
		return "", fmt.Errorf("docker: export %s: a %s run's files are not carried", ref, c.Config.Labels[labelMachineKind])
	case c.State != nil && (c.State.Running || c.State.Paused || c.State.Restarting):
		return "", fmt.Errorf("docker: export %s: the sandbox is not stopped", ref)
	}
	return c.ID, nil
}

// stillExportable fails an export whose container is gone, replaced, or no longer stopped since its start.
func (d *Driver) stillExportable(ctx context.Context, ref, id string) error {
	now, err := d.exportable(ctx, ref)
	switch {
	case err != nil:
		return fmt.Errorf("docker: export %s: the sandbox changed during the export: %w", ref, err)
	case now != id:
		return fmt.Errorf("docker: export %s: the sandbox was replaced during the export", ref)
	}
	return nil
}

// exportTo writes ref's carried files to out as a movepack stream, its roots in the stream's order. id is
// the container the export started on; it is inspected again on every missing root and at the end.
func (d *Driver) exportTo(ctx context.Context, ref, id string, set runner.CarriedSet, out io.Writer) error {
	w, err := movepack.NewWriter(out, set)
	if err != nil {
		return err
	}
	roots := slices.Clone(set.Roots)
	// The stream's order compares paths a component at a time; "/" mapped below every byte does that.
	slices.SortFunc(roots, func(a, b string) int {
		return strings.Compare(strings.ReplaceAll(a, "/", "\x00"), strings.ReplaceAll(b, "/", "\x00"))
	})
	flatten := func(abs string) (io.ReadCloser, int64, error) { return d.exportFile(ctx, ref, abs) }
	for _, root := range roots {
		res, err := d.cli.CopyFromContainer(ctx, ref, client.CopyFromContainerOptions{SourcePath: root})
		if isNotFound(err) {
			if err := d.stillExportable(ctx, ref, id); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("docker: export %s of %s: %w", root, ref, err)
		}
		err = movepack.FromTar(w, tar.NewReader(res.Content), root, path.Base(root), flatten)
		if cerr := res.Content.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	// Before the manifest: a stream is never ended short by a container that went away under it.
	if err := d.stillExportable(ctx, ref, id); err != nil {
		return err
	}
	_, err = w.Close()
	return err
}

// exportFile opens one regular file of ref by its absolute path: a hardlink's target, carried as a file.
func (d *Driver) exportFile(ctx context.Context, ref, abs string) (io.ReadCloser, int64, error) {
	res, err := d.cli.CopyFromContainer(ctx, ref, client.CopyFromContainerOptions{SourcePath: abs})
	if err != nil {
		return nil, 0, err
	}
	th, err := tar.NewReader(res.Content).Next()
	if err == nil && th.Typeflag != tar.TypeReg {
		err = fmt.Errorf("%s is not a regular file", abs)
	}
	if err != nil {
		_ = res.Content.Close()
		return nil, 0, err
	}
	// Next left the archive at the file's content, which the size bounds.
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(res.Content, th.Size), res.Content}, th.Size, nil
}

// ImportSandbox creates spec's sandbox with its agent created and not started and no proxy, then writes
// carried into it through the daemon's archive API, so nothing inside the container races the extraction
// and Wardyn opens no file in it. The stream is read by movepack's reader, which refuses every hostile
// entry before its bytes reach the archive, and re-emitted as a tar this driver writes: owner and group the
// agent's, mode masked to 0777, and the symlinks written after every other entry, once the stream's
// manifest has matched. The import also writes the booted marker, so the run's seed does not run again.
func (d *Driver) ImportSandbox(ctx context.Context, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	none := runner.CarriedManifest{}
	if err := runner.CheckImportSpec(spec); err != nil {
		return runner.Sandbox{}, none, fmt.Errorf("%w: %w", runner.ErrMoveCreate, err)
	}
	if spec.MachineKind.Normalize() != "" {
		return runner.Sandbox{}, none, fmt.Errorf("%w: docker: a moved run's sandbox is a container, never a %s", runner.ErrMoveCreate, spec.MachineKind)
	}
	if err := runner.CheckCarriedLimit(maxBytes); err != nil {
		return runner.Sandbox{}, none, fmt.Errorf("%w: %w", runner.ErrMoveCreate, err)
	}
	rd, err := movepack.NewReader(carried, movepack.Options{Set: set, Limits: movepack.Limits{MaxBytes: maxBytes}})
	if err != nil {
		return runner.Sandbox{}, none, fmt.Errorf("%w: %w", runner.ErrMoveCreate, err)
	}
	defer rd.Close()
	sb, containerID, undo, err := d.createImported(ctx, spec)
	if err != nil {
		return runner.Sandbox{}, none, fmt.Errorf("%w: %w", runner.ErrMoveCreate, err)
	}
	pr, pw := io.Pipe()
	wrote := make(chan error, 1)
	go func() {
		err := reemit(pw, rd)
		pw.CloseWithError(err)
		wrote <- err
	}()
	_, err = d.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: "/", Content: pr})
	// Ends the re-emit if the daemon stopped reading first.
	_ = pr.CloseWithError(errors.New("docker: the archive call ended"))
	// Both errors are kept: the daemon's, and the stream's refusal, which may be why the daemon stopped.
	err = errors.Join(err, <-wrote)
	if err != nil {
		return runner.Sandbox{}, none, fmt.Errorf("%w: docker: import into %s: %w", runner.ErrMoveFiles, sb.Ref, undo(err))
	}
	return sb, rd.Manifest(), nil
}

// reemit writes the entries rd reads as a tar for the daemon to extract at "/", then the booted marker.
// It holds the symlinks back and writes them only after rd has read the stream's manifest and matched it.
func reemit(out io.Writer, rd *movepack.Reader) error {
	tw := tar.NewWriter(out)
	now := time.Now().Truncate(time.Second)
	hdr := func(name string, mode int64) *tar.Header {
		return &tar.Header{Name: name, Mode: mode & 0o777, Uid: runner.AgentUID, Gid: runner.AgentUID, ModTime: now}
	}
	var links []*tar.Header
	held := 0
	for {
		h, content, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		th := hdr(h.Path, h.Mode)
		switch h.Type {
		case movepack.TypeDir:
			th.Typeflag, th.Name = tar.TypeDir, h.Path+"/"
		case movepack.TypeFile:
			th.Typeflag, th.Size = tar.TypeReg, h.Size
		case movepack.TypeSymlink:
			if held += len(h.Path) + len(h.Link); held > maxImportLinkBytes {
				return fmt.Errorf("%w: over %d bytes of symlinks", movepack.ErrCarriedTooLarge, maxImportLinkBytes)
			}
			th.Typeflag, th.Linkname = tar.TypeSymlink, h.Link
			links = append(links, th)
			continue
		default:
			return fmt.Errorf("%w: entry %q has type %q", movepack.ErrArchiveRefused, h.Path, h.Type)
		}
		if err := tw.WriteHeader(th); err != nil {
			return err
		}
		if content != nil {
			if _, err := io.CopyN(tw, content, h.Size); err != nil {
				return err
			}
		}
	}
	for _, th := range links {
		if err := tw.WriteHeader(th); err != nil {
			return err
		}
	}
	marker := strings.TrimPrefix(bootedMarker, "/")
	dir := hdr(path.Dir(marker)+"/", 0o755)
	dir.Typeflag = tar.TypeDir
	file := hdr(marker, 0o644)
	file.Typeflag = tar.TypeReg
	for _, th := range []*tar.Header{dir, file} {
		if err := tw.WriteHeader(th); err != nil {
			return err
		}
	}
	return tw.Close()
}
