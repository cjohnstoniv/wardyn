// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// The profiles edit-base offers the person. macOS installs a profile only with a person's approval, made
// from the desktop session as wardyn-admin, so each profile still staged is staged again for the window
// (root's, 0600 in its 0700 stage directory) and copied to wardyn-admin's own directory to open; the
// recheck removes both and checks each is gone. Only paths are logged or printed, never a profile's
// contents.

// offerProfiles stages again each profile dir's tools.json holds as staged (restageProfiles) and gives
// wardyn-admin its copy of each (copyForAdmin), in edit-base's first boot. It returns the copies' paths.
func offerProfiles(ctx context.Context, g ToolsGuest, dir string, l BaseList, log *slog.Logger) ([]string, error) {
	staged, err := restageProfiles(ctx, g, dir, l, log)
	if err != nil || len(staged) == 0 {
		return nil, err
	}
	return copyForAdmin(ctx, g, staged)
}

// restageProfiles stages again, as prepare does, each profile dir's tools.json holds as staged whose file
// the install list l still names with the same pin, and returns their guest paths: the last recheck
// removed them. A profile with no such file on this Mac is not staged and keeps its state; one the list
// pins differently is logged, by its path.
func restageProfiles(ctx context.Context, g ToolsGuest, dir string, l BaseList, log *slog.Logger) ([]string, error) {
	rec, err := ReadToolsRecord(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	files := map[string]BaseItem{}
	for _, it := range l.Items {
		if it.Kind == ItemKindProfile {
			files[it.Name] = it
		}
	}
	var out []string
	for _, r := range rec.Items {
		it, ok := files[r.Name]
		if r.Kind != ItemKindProfile || r.State != ItemStaged || !ok {
			continue
		}
		if it.SHA256 != r.SHA256 {
			log.Warn("macvm: edit-base does not offer a staged profile: the install list pins another file than the base was built with; a tools update stages it",
				"path", it.Path)
			continue
		}
		if _, err := os.Stat(it.Path); err != nil {
			continue
		}
		if _, err := installItem(ctx, g, it); err != nil {
			return nil, fmt.Errorf("macvm: base item %s (%s): %w", it.Name, it.Kind, err)
		}
		out = append(out, guestagent.ItemDir(it.Name)+"/"+guestagent.ItemProfile)
	}
	return out, nil
}

// copyForAdmin copies each staged profile into guestagent.MacAdminProfileDir, made afresh 0700, keeping
// its 0600, and hands the copies and then the directory to wardyn-admin. It returns the copies' paths.
func copyForAdmin(ctx context.Context, g ToolsGuest, staged []string) ([]string, error) {
	dir := guestagent.MacAdminProfileDir
	if err := runItem(ctx, g, "/bin/rm", "-rf", dir); err != nil {
		return nil, err
	}
	if err := runItem(ctx, g, "/bin/mkdir", "-m", "0700", dir); err != nil {
		return nil, err
	}
	copies := make([]string, 0, len(staged))
	for _, p := range staged {
		c := guestagent.AdminProfile(filepath.Base(filepath.Dir(p)))
		if err := runItem(ctx, g, "/usr/bin/ditto", p, c); err != nil {
			return nil, err
		}
		if err := runItem(ctx, g, "/usr/sbin/chown", "-h", guestagent.MacAdminUser, c); err != nil {
			return nil, err
		}
		copies = append(copies, c)
	}
	if err := runItem(ctx, g, "/usr/sbin/chown", "-h", guestagent.MacAdminUser, dir); err != nil {
		return nil, err
	}
	return copies, nil
}

// removeProfiles is edit-base's recheck, the first session in the guest after its window closes: it
// removes wardyn-admin's copies with their directory (copied: this edit made some), then each staged
// profile's stage directory (StagedProfiles' guest paths), checking each is gone. A profile is installed
// by then, or still staged in the record for the next edit: no session in the guest while the window is
// open could see an approval sooner.
func removeProfiles(ctx context.Context, g ToolsGuest, copied bool, staged []string) error {
	var dirs []string
	if copied {
		dirs = append(dirs, guestagent.MacAdminProfileDir)
	}
	for _, p := range staged {
		dirs = append(dirs, filepath.Dir(p))
	}
	for _, d := range dirs {
		if err := runItem(ctx, g, "/bin/rm", "-rf", d); err != nil {
			return err
		}
		_, exit, err := g.Run(ctx, []string{"/usr/bin/stat", d})
		if err != nil {
			return err
		}
		if exit == 0 {
			return fmt.Errorf("%s is still there after its removal", d)
		}
	}
	return nil
}
