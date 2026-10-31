// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A Mac's other macOS bases. Each is derived from its first base, Standard (BaseDir(0)): an APFS clone of
// Standard and a stage-2 boot that installs the derived base's own install list, keeping Standard's tools.
// When Standard changes, its stamp does, and each derived base is derived again from it.

// StandardBase is the name of a Mac's first base, the one prepare builds (MAC-B04).
const StandardBase = "Standard"

// FlagVariant is update-tools' derived base: with it, update-tools derives that base from Standard, with
// --base-file naming the derived base's own install list.
const FlagVariant = "variant"

// ValidBaseName says whether name can name a derived base: an install list item's name form, so never
// Standard and never a path.
func ValidBaseName(name string) bool { return guestagent.ItemNamePattern.MatchString(name) }

// NamedBaseDir is the dir of the base a run names: Standard for "" or StandardBase, else the derived base.
func (l Layout) NamedBaseDir(name string) (string, error) {
	switch {
	case name == "" || name == StandardBase:
		return l.BaseDir(0), nil
	case ValidBaseName(name):
		return l.VariantDir(name), nil
	}
	return "", fmt.Errorf("macvm: %q is not a macOS base name", name)
}

// BaseVariant is one derived base the operator defines: its name and its own install list.
type BaseVariant struct {
	Name string
	List BaseList
}

// LoadBaseVariants reads the derived bases the runner setting macos_vm_bases_dir defines: each regular
// <name>.json in dir is the install list (in macos_vm_base_file's form) of the base <name>, in name order;
// other files are ignored. A list cannot hold a profile: a profile waits for approval in macos-vm
// edit-base, which edits Standard only.
func LoadBaseVariants(dir string) ([]BaseVariant, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("macvm: the macOS bases directory: %w", err)
	}
	var out []BaseVariant
	for _, de := range des {
		name, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok || !de.Type().IsRegular() {
			continue
		}
		if !ValidBaseName(name) {
			return nil, fmt.Errorf("macvm: the macOS bases directory %s: %q is not a base name (lowercase letters, digits, '.', '_' and '-')", dir, name)
		}
		l, err := LoadBaseList(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		if i := slices.IndexFunc(l.Items, func(it BaseItem) bool { return it.Kind == ItemKindProfile }); i >= 0 {
			return nil, fmt.Errorf("macvm: the macOS base %s: item %q is a profile, which only Standard's list (macos_vm_base_file) can hold", name, l.Items[i].Name)
		}
		out = append(out, BaseVariant{Name: name, List: l})
	}
	return out, nil
}

// BaseStamp is a hash of the base in dir as it is now: its manifest, its tools.json and its last edit. A
// prepare, a tools update and an edit each change it.
func BaseStamp(dir string) (string, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, f := range []string{ManifestFile, ToolsRecordFile} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	if at := BaseEditedAt(dir, m); at != nil {
		h.Write([]byte(at.UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VariantCurrent says whether the derived base in dir is ready, was cloned from the Standard whose stamp
// is stamp, and holds the install list whose digest is digest. One that is not is derived again.
func VariantCurrent(dir, stamp, digest string) bool {
	m, err := ReadyBase(dir)
	return err == nil && m.DerivedFrom == stamp && !ItemsChanged(dir, digest)
}

// stage2Tools is stage 2's tools step: a derivation's boot (in.Kept) installs no tools and goes on from
// those its clone of Standard holds; every other boot installs them.
func stage2Tools(ctx context.Context, g ToolsGuest, in Stage2Input, o ToolsOptions) (ToolsResult, error) {
	k := in.Kept
	if k == nil {
		return installTools(ctx, g, o)
	}
	res := ToolsResult{Harnesses: slices.Clone(k.Harnesses), Withheld: map[string]error{}, Pins: map[string]PinRecord{},
		Tried: map[string]PinRecord{}, AgentRun: k.AgentRun, Items: slices.Clone(k.Items)}
	for id, p := range k.Pins {
		res.Pins[id], res.Tried[id] = p, p
	}
	for id, w := range k.Withheld {
		res.Withheld[id], res.Tried[id] = errors.New(w.Reason), w.PinRecord
	}
	return res, nil
}

// DeriveBase derives the base name from Standard (o.Slot's base): it clones Standard, runs o.Stage on the
// clone with Standard's tools kept, so the boot installs only o.Stage's install list (the derived base's
// own), and swaps the clone in as the derived base, as UpdateTools swaps in an update. It holds the
// derived base's update lock throughout, and Standard's base lock shared while it clones. A derived base
// already current (VariantCurrent with o.ItemsDigest) is left as it is unless o.Force. A failed or
// interrupted derivation leaves the derived base as it was. It reports whether anything changed.
func DeriveBase(ctx context.Context, o UpdateOptions, name string) (Manifest, bool, error) {
	if o.Engine == nil || o.Power == nil || o.Stage == nil || o.Layout.Root == "" {
		return Manifest{}, false, errors.New("macvm: a derivation needs an engine, a power source, a stage 2 and a root")
	}
	if !ValidBaseName(name) {
		return Manifest{}, false, fmt.Errorf("macvm: %q is not a macOS base name", name)
	}
	o.setDefaults()
	std, dir := o.Layout.BaseDir(o.Slot), o.Layout.VariantDir(name)
	if err := mkdirPrivate(filepath.Dir(dir)); err != nil {
		return Manifest{}, false, err
	}
	nb, done, err := beginUpdate(ctx, dir, o.Log)
	if err != nil {
		return Manifest{}, false, err
	}
	release := func() {}
	defer func() { done(); release() }()
	// Standard's stamp and tools are read before its clone: a Standard swapped in between leaves the
	// derived base stale, so it is derived again, and never current with another Standard's disk.
	stamp, err := BaseStamp(std)
	if err != nil {
		return Manifest{}, false, err
	}
	kept, err := ReadToolsRecord(std)
	if err != nil {
		return Manifest{}, false, fmt.Errorf("macvm: Standard's tools record: %w", err)
	}
	if !o.Force && VariantCurrent(dir, stamp, o.ItemsDigest) {
		m, err := ReadManifest(dir)
		return m, false, err
	}
	if err := thermalGate(o.Engine); err != nil {
		return Manifest{}, false, err
	}
	if o.Admit != nil {
		r, err := o.Admit(ctx)
		if err != nil {
			return Manifest{}, false, err
		}
		release = r
	}
	d := &downloader{power: o.Power, poll: o.PowerPoll, paused: func(on bool) {
		ev := PrepareEvent{Phase: PhaseTools, Paused: on}
		if on {
			ev.Detail = "waiting for AC power"
		}
		o.Progress(ev)
	}}
	if err := d.waitAC(ctx); err != nil {
		return Manifest{}, false, err
	}
	o.Progress(PrepareEvent{Phase: PhaseTools})
	m, id, err := cloneReadyBase(o.Engine, std, nb, IdentityTravel)
	if err != nil {
		return m, false, err
	}
	o.Progress(PrepareEvent{Phase: PhaseTools, Fraction: 0.1})
	spec := MachineSpec{
		CPUs: o.CPUs, MemoryBytes: o.MemoryMiB * mib, MinCPUs: m.MinCPUs, MinMemoryBytes: m.MinMemoryMiB * mib,
		HardwareModel: m.HardwareModel, MachineIdentifier: id.MachineIdentifier,
		DiskPath: filepath.Join(nb, DiskFile), AuxPath: filepath.Join(nb, AuxFile),
	}
	res, err := o.Stage.Run(ctx, Stage2Input{Spec: spec, Dir: nb, Kept: &kept})
	if res.ToolsDial != "" {
		o.Progress(PrepareEvent{Phase: PhaseTools, ToolsDial: res.ToolsDial})
	}
	if err != nil {
		return m, false, err
	}
	o.Progress(PrepareEvent{Phase: PhaseTools, Fraction: 0.9})
	m.Harnesses, m.HelperVersion, m.DerivedFrom = res.Harnesses, o.HelperVersion, stamp
	if err := writeToolsRecord(nb, res, m.AgentVersion, o.Now()); err != nil {
		return m, false, err
	}
	if err := writeJSONAtomic(filepath.Join(nb, ManifestFile), m, 0o600); err != nil {
		return m, false, err
	}
	// The first derivation swaps with an empty dir, which the staging dir then holds and done deletes.
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := mkdirPrivate(dir); err != nil {
			return m, false, err
		}
	}
	if err := swapBase(ctx, nb, dir); err != nil {
		return m, false, err
	}
	o.Progress(PrepareEvent{Phase: PhaseReady, Fraction: 1})
	return m, true, nil
}
