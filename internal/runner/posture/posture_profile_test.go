// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package posture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/managedprefs"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeProfile stands the managed preferences profile in: the plist is a JSON object of its keys (the fake
// extract reads that), at the device-wide path under a temporary root. It returns the plist's path.
func fakeProfile(t *testing.T, keys map[string]string, mode os.FileMode) string {
	t.Helper()
	root := t.TempDir()
	prevOS, prevRoot, prevExtract := profileOS, profileRoot, profileExtract
	t.Cleanup(func() { profileOS, profileRoot, profileExtract = prevOS, prevRoot, prevExtract })
	profileOS, profileRoot = "darwin", root
	profileExtract = func(file, key string) (string, error) {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			return "", err
		}
		return m[key], nil
	}
	path := filepath.Join(root, managedprefs.Domain+".plist")
	b, _ := json.Marshal(keys)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// With no posture file, the profile's MDMManaged and DiskEncrypted are the MDM's word, reported as the file's
// evidence (mdm_file), because that is what the organisation's macOS gate accepts.
func TestPosture_ProfileKeysWhenThereIsNoFile(t *testing.T) {
	uid := uint32(os.Getuid())
	fakeProfile(t, map[string]string{"MDMManaged": "true", "DiskEncrypted": "true"}, 0o644)
	none := filepath.Join(t.TempDir(), "posture.json")
	if p := read(none, uid); p.Evidence != types.PostureEvidenceMDMFile || !p.MDMManaged || !p.DiskEncrypted {
		t.Fatalf("profile = %+v, want mdm_file, managed and encrypted", p)
	}
}

// A profile that sets MDMManaged and not DiskEncrypted leaves disk encryption to the runner's own probe.
func TestPosture_ProfileWithoutDiskEncryptedKeepsTheProbe(t *testing.T) {
	fakeProfile(t, map[string]string{"MDMManaged": "true"}, 0o644)
	p := read(filepath.Join(t.TempDir(), "posture.json"), uint32(os.Getuid()))
	if p.Evidence != types.PostureEvidenceMDMFile || !p.MDMManaged || p.DiskEncrypted != diskEncrypted() {
		t.Fatalf("profile = %+v, want mdm_file, managed, disk as probed (%v)", p, diskEncrypted())
	}
}

// The posture file outranks the profile, and a file that is there but unusable is not worked around by it.
func TestPosture_FileOutranksProfile(t *testing.T) {
	uid := uint32(os.Getuid())
	fakeProfile(t, map[string]string{"MDMManaged": "true", "DiskEncrypted": "true"}, 0o644)
	path := filepath.Join(t.TempDir(), "posture.json")
	if err := os.WriteFile(path, []byte(`{"mdm_managed":false,"disk_encrypted":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := read(path, uid); p.Evidence != types.PostureEvidenceMDMFile || p.MDMManaged || p.DiskEncrypted {
		t.Fatalf("file and profile = %+v, want the file's false", p)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if p := read(path, uid); p.Evidence != types.PostureEvidenceOSProbe || p.MDMManaged {
		t.Fatalf("unusable file and a profile = %+v, want the probe", p)
	}
}

// A profile is trusted on the terms of the file: another owner, a group- or world-writable plist, a value that
// is not a boolean, a key left out and a host that is not a Mac all leave the runner's own probe.
func TestPosture_ProfileIgnoredWhenNotTrusted(t *testing.T) {
	uid := uint32(os.Getuid())
	none := filepath.Join(t.TempDir(), "posture.json")
	yes := map[string]string{"MDMManaged": "true"}
	for name, run := range map[string]func() types.RunnerPosture{
		"another owner": func() types.RunnerPosture { fakeProfile(t, yes, 0o644); return read(none, uid+1) },
		"group-writable": func() types.RunnerPosture {
			fakeProfile(t, yes, 0o664)
			return read(none, uid)
		},
		"world-writable": func() types.RunnerPosture {
			fakeProfile(t, yes, 0o646)
			return read(none, uid)
		},
		"not a boolean": func() types.RunnerPosture {
			fakeProfile(t, map[string]string{"MDMManaged": "maybe"}, 0o644)
			return read(none, uid)
		},
		"no MDMManaged": func() types.RunnerPosture {
			fakeProfile(t, map[string]string{"DiskEncrypted": "true"}, 0o644)
			return read(none, uid)
		},
		"not a Mac": func() types.RunnerPosture {
			fakeProfile(t, yes, 0o644)
			profileOS = "linux"
			return read(none, uid)
		},
	} {
		if p := run(); p.Evidence != types.PostureEvidenceOSProbe || p.MDMManaged {
			t.Errorf("%s: %+v, want the profile ignored", name, p)
		}
	}
}

// A profile under the person's own directory is not the MDM's word: only the device-wide file counts.
func TestPosture_UserScopedProfileIsIgnored(t *testing.T) {
	uid := uint32(os.Getuid())
	path := fakeProfile(t, map[string]string{"MDMManaged": "false"}, 0o644)
	mine := managedprefs.Files(filepath.Dir(path), managedprefs.Domain)
	if len(mine) != 2 {
		t.Skip("no current user to scope a profile to")
	}
	if err := os.MkdirAll(filepath.Dir(mine[0]), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"MDMManaged": "true", "DiskEncrypted": "true"})
	if err := os.WriteFile(mine[0], b, 0o600); err != nil {
		t.Fatal(err)
	}
	if p := read(filepath.Join(t.TempDir(), "posture.json"), uid); p.MDMManaged {
		t.Fatalf("posture = %+v, want the user-scoped profile ignored", p)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if p := read(filepath.Join(t.TempDir(), "posture.json"), uid); p.MDMManaged || p.Evidence == types.PostureEvidenceMDMFile {
		t.Fatalf("posture = %+v, want no profile claim from a user-scoped file alone", p)
	}
}
