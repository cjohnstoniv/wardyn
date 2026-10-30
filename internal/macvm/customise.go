// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// FlagBaseFile is prepare's and update-tools' install list (the runner setting macos_vm_base_file).
const FlagBaseFile = "base-file"

// Base item kinds.
const (
	ItemKindPkg     = "pkg"
	ItemKindApp     = "app"
	ItemKindFile    = "file"
	ItemKindProfile = "profile"
)

// Item states: installed, or a profile staged for the person's approval in macos-vm edit-base.
const (
	ItemInstalled = "installed"
	ItemStaged    = "staged"
)

// profileID is a configuration profile's PayloadIdentifier.
var profileID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// maxBaseList bounds the install list file.
const maxBaseList = 1 << 20

// BaseList is the operator's install list, the file the locked runner setting macos_vm_base_file names:
// the items stage 2 installs into the base as admin, at prepare and at every tools update. Each item's
// file is on this Mac, pinned by its SHA-256, staged in the guest in a root-only directory and installed
// with fixed argv forms (guestagent itemArgvAllowed).
type BaseList struct {
	Version int        `json:"version"`
	Items   []BaseItem `json:"items"`
}

// BaseItem is one item of the list.
type BaseItem struct {
	Kind string `json:"kind"`
	// Name is the item's own name (guestagent.ItemNamePattern), unique in the list.
	Name string `json:"name"`
	// Path is the item's file on this Mac, and SHA256 the hash it must have.
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	// Dest is a file item's path in the guest (guestagent.ItemDestAllowed).
	Dest string `json:"dest,omitempty"`
	// App is the bundle an app item's .dmg holds ("Name.app"); an app item's .zip holds its bundle at the
	// top and needs none.
	App string `json:"app,omitempty"`
	// Identifier is a profile item's PayloadIdentifier, which `profiles list` reads back once it is
	// installed.
	Identifier string `json:"identifier,omitempty"`
}

// LoadBaseList reads and checks the install list at path.
func LoadBaseList(path string) (BaseList, error) {
	f, err := os.Open(path)
	if err != nil {
		return BaseList{}, fmt.Errorf("macvm: the macOS base install list: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBaseList+1))
	if err != nil {
		return BaseList{}, fmt.Errorf("macvm: the macOS base install list: %w", err)
	}
	if len(b) > maxBaseList {
		return BaseList{}, fmt.Errorf("macvm: the macOS base install list %s is over %d bytes", path, maxBaseList)
	}
	l, err := ParseBaseList(b)
	if err != nil {
		return BaseList{}, fmt.Errorf("macvm: the macOS base install list %s: %w", path, err)
	}
	return l, nil
}

// ParseBaseList parses an install list and checks every item, refusing an unknown field, kind or version.
func ParseBaseList(b []byte) (BaseList, error) {
	var l BaseList
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return BaseList{}, err
	}
	if l.Version != 1 {
		return BaseList{}, fmt.Errorf("version %d is not 1", l.Version)
	}
	seen := map[string]bool{}
	for i, it := range l.Items {
		if err := it.check(); err != nil {
			return BaseList{}, fmt.Errorf("item %d (%q): %w", i+1, it.Name, err)
		}
		if seen[it.Name] {
			return BaseList{}, fmt.Errorf("item %d: the name %q is used twice", i+1, it.Name)
		}
		seen[it.Name] = true
	}
	return l, nil
}

func (it BaseItem) check() error {
	switch {
	case !guestagent.ItemNamePattern.MatchString(it.Name):
		return errors.New("a name is lower-case letters, digits, '.', '_' and '-', at most 64")
	case !filepath.IsAbs(it.Path):
		return errors.New("path must be an absolute path on this Mac")
	case !sha256Hex.MatchString(it.SHA256):
		return errors.New("sha256 must be 64 lower-case hex digits")
	}
	ext := strings.ToLower(filepath.Ext(it.Path))
	if it.Kind != ItemKindProfile && it.Identifier != "" {
		return errors.New("only a profile item takes an identifier")
	}
	switch it.Kind {
	case ItemKindPkg:
		if ext != ".pkg" || it.Dest != "" || it.App != "" {
			return errors.New("a pkg item's path ends in .pkg, and it takes no dest or app")
		}
	case ItemKindApp:
		if it.Dest != "" || !(ext == ".zip" && it.App == "" || ext == ".dmg" && guestagent.ItemAppPattern.MatchString(it.App)) {
			return errors.New("an app item is a .zip of its bundle, or a .dmg with app naming the bundle in it (Name.app); it takes no dest")
		}
	case ItemKindFile:
		if it.App != "" {
			return errors.New("a file item takes no app")
		}
		if err := guestagent.ItemDestAllowed(it.Dest); err != nil {
			return fmt.Errorf("dest %w", err)
		}
	case ItemKindProfile:
		if ext != ".mobileconfig" || it.Dest != "" || it.App != "" || !profileID.MatchString(it.Identifier) {
			return errors.New("a profile item's path ends in .mobileconfig, and it takes its PayloadIdentifier as identifier and no dest or app")
		}
	default:
		return fmt.Errorf("kind %q is not one of pkg, app, file, profile", it.Kind)
	}
	return nil
}

// Digest is the list's digest, which tools.json records: a tools update runs when it changes. It is a
// hash of the parsed items, pins included; "" for no items.
func (l BaseList) Digest() string {
	if len(l.Items) == 0 {
		return ""
	}
	b, _ := json.Marshal(l.Items)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ItemsChanged says whether the base in baseDir records an install list digest other than digest; a base
// with no tools.json was built with no list.
func ItemsChanged(baseDir, digest string) bool {
	rec, _ := ReadToolsRecord(baseDir)
	return rec.ItemsDigest != digest
}

// ItemRecord is one item as stage 2 left it, in tools.json: never a secret, and never a path on this Mac.
type ItemRecord struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	State  string `json:"state"`
	// Identifier is a profile's PayloadIdentifier.
	Identifier string `json:"identifier,omitempty"`
}

// installItems installs the list's items in order, after the harnesses and before the self-test, which
// then runs on the base as a run gets it. An item that fails stops stage 2: prepare publishes no base, and
// a tools update keeps the old one. Its command's output is not kept, since an installer's may echo what
// it was given.
func installItems(ctx context.Context, g ToolsGuest, o ToolsOptions, res *ToolsResult) error {
	res.ItemsDigest = o.Items.Digest()
	if len(o.Items.Items) == 0 {
		return nil
	}
	if err := runItem(ctx, g, "/bin/mkdir", "-p", "-m", "0700", guestagent.MacItemStageDir); err != nil {
		return fmt.Errorf("macvm: the base items' stage directory: %w", err)
	}
	for _, it := range o.Items.Items {
		rec, err := installItem(ctx, g, it)
		if err != nil {
			return fmt.Errorf("macvm: base item %s (%s): %w", it.Name, it.Kind, err)
		}
		res.Items = append(res.Items, rec)
	}
	return readProfiles(ctx, g, res.Items)
}

// readProfiles reads back which profile items `profiles list` names, marking each installed or staged.
// A profile is staged at prepare and approved by the person in macos-vm edit-base: macOS installs one
// only with that approval, or from MDM.
func readProfiles(ctx context.Context, g ToolsGuest, items []ItemRecord) error {
	if !slices.ContainsFunc(items, func(r ItemRecord) bool { return r.Kind == ItemKindProfile }) {
		return nil
	}
	out, exit, err := g.Run(ctx, []string{"/usr/bin/profiles", "list"})
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if _, id, ok := strings.Cut(line, "attribute: profileIdentifier:"); ok {
			installed[strings.TrimSpace(id)] = exit == 0
		}
	}
	for i, r := range items {
		if r.Kind == ItemKindProfile {
			items[i].State = map[bool]string{true: ItemInstalled, false: ItemStaged}[installed[r.Identifier]]
		}
	}
	return nil
}

// StagedProfiles is the guest path of each profile item the base in dir holds staged and not yet
// installed: what macos-vm edit-base offers the person to approve.
func StagedProfiles(dir string) ([]string, error) {
	rec, err := ReadToolsRecord(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rec.Items {
		if r.Kind == ItemKindProfile && r.State == ItemStaged {
			out = append(out, guestagent.ItemDir(r.Name)+"/"+guestagent.ItemProfile)
		}
	}
	return out, nil
}

// ReadBackItems reads the base items' state again in g, a boot of the base in dir, and records it in dir's
// tools.json: macos-vm edit-base's recheck calls it once the person has approved the staged profiles.
func ReadBackItems(ctx context.Context, g ToolsGuest, dir string) error {
	rec, err := ReadToolsRecord(dir)
	if err != nil {
		return err
	}
	if err := readProfiles(ctx, g, rec.Items); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(dir, ToolsRecordFile), rec, 0o600)
}

func installItem(ctx context.Context, g ToolsGuest, it BaseItem) (rec ItemRecord, err error) {
	rec = ItemRecord{Kind: it.Kind, Name: it.Name, SHA256: it.SHA256, State: ItemInstalled, Identifier: it.Identifier}
	if it.Kind == ItemKindFile {
		if err := runItem(ctx, g, "/bin/mkdir", "-p", "-m", "0755", filepath.Dir(it.Dest)); err != nil {
			return rec, err
		}
		return rec, pushItem(ctx, g, it.Path, it.SHA256, it.Dest, 0o644)
	}
	dir := guestagent.ItemDir(it.Name)
	if err := runItem(ctx, g, "/bin/mkdir", "-p", "-m", "0700", dir); err != nil {
		return rec, err
	}
	if it.Kind == ItemKindProfile {
		// Kept staged for the person's approval in edit-base; readProfiles reads back whether it is installed.
		return rec, pushItem(ctx, g, it.Path, it.SHA256, dir+"/"+guestagent.ItemProfile, 0o600)
	}
	// The stage directory goes after the install, whatever its end.
	defer func() {
		if rerr := runItem(ctx, g, "/bin/rm", "-rf", dir); err == nil && rerr != nil {
			err = fmt.Errorf("removing its stage directory: %w", rerr)
		}
	}()
	switch {
	case it.Kind == ItemKindPkg:
		pkg := dir + "/" + guestagent.ItemPkg
		if err := pushItem(ctx, g, it.Path, it.SHA256, pkg, 0o600); err != nil {
			return rec, err
		}
		return rec, runItem(ctx, g, "/usr/sbin/installer", "-pkg", pkg, "-target", "/")
	case it.App == "":
		zip := dir + "/" + guestagent.ItemZip
		if err := pushItem(ctx, g, it.Path, it.SHA256, zip, 0o600); err != nil {
			return rec, err
		}
		return rec, runItem(ctx, g, "/usr/bin/ditto", "-x", "-k", zip, guestagent.MacApplications)
	}
	dmg, mnt := dir+"/"+guestagent.ItemDmg, dir+"/"+guestagent.ItemMount
	if err := pushItem(ctx, g, it.Path, it.SHA256, dmg, 0o600); err != nil {
		return rec, err
	}
	if err := runItem(ctx, g, "/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, dmg); err != nil {
		return rec, err
	}
	err = runItem(ctx, g, "/usr/bin/ditto", mnt+"/"+it.App, guestagent.MacApplications+"/"+it.App)
	if derr := runItem(ctx, g, "/usr/bin/hdiutil", "detach", "-force", mnt); err == nil {
		err = derr
	}
	return rec, err
}

// pushItem checks the file at path against its pin and pushes it to dest through the same handle.
func pushItem(ctx context.Context, g ToolsGuest, path, pin, dest string, mode os.FileMode) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if ok, err := hasSHA256(f, pin); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s does not match its pinned SHA-256", path)
	}
	return g.Push(ctx, dest, mode, f, fi.Size())
}

// runItem runs one item form and fails on a non-zero exit, naming the command and not its output.
func runItem(ctx context.Context, g ToolsGuest, argv ...string) error {
	_, exit, err := g.Run(ctx, argv)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("%s %s exited %d", filepath.Base(argv[0]), argv[1], exit)
	}
	return nil
}
