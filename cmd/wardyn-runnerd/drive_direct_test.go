// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

const (
	driveDirectTestObject = "wardyn-drive-test"
	// driveDirectTestChildEnv makes the test binary the `drive open --direct` command a test then kills.
	driveDirectTestChildEnv = "WARDYN_TEST_DRIVE_DIRECT_CHILD"
)

// driveDirectFakeHdiutil is hdiutil on PATH. Files in $FAKE steer it: it records each call, reports an attach
// by a file named attached, and fails or hangs when told to. An attach told to wait stays running, as the child
// of a killed command does, and then attaches.
const driveDirectFakeHdiutil = `#!/bin/sh
echo "$*" >> "$FAKE/calls"
plist_head='<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>framework</key><string>1</string>'
case "$1" in
attach)
	[ -e "$IMG.mounted" ] && cp "$IMG.mounted" "$FAKE/marker-at-attach"
	if [ -e "$FAKE/attach-wait" ]; then
		i=0
		while [ -e "$FAKE/attach-wait" ] && [ $i -lt 150 ]; do sleep 0.2; i=$((i+1)); done
	fi
	if [ -e "$FAKE/attach-fail" ]; then echo "attach failed" >&2; exit 1; fi
	touch "$FAKE/attached"
	if [ -e "$FAKE/attach-garbage" ]; then echo "not a plist"; exit 0; fi
	echo "$plist_head<key>system-entities</key><array>"
	echo "<dict><key>dev-entry</key><string>/dev/disk9</string></dict>"
	echo "<dict><key>dev-entry</key><string>/dev/disk9s1</string></dict>"
	echo "<dict><key>dev-entry</key><string>/dev/disk9s2</string><key>mount-point</key><string>/Volumes/scratch</string></dict>"
	echo "</array></dict></plist>"
	;;
info)
	if [ -e "$FAKE/info-fail" ]; then echo "info failed" >&2; exit 1; fi
	if [ -e "$FAKE/info-garbage" ]; then echo "<plist><dict><key>images"; exit 0; fi
	echo "$plist_head<key>images</key><array>"
	echo "<dict><key>image-path</key><string>/other/image.dmg</string><key>system-entities</key><array><dict><key>dev-entry</key><string>/dev/disk7</string></dict></array></dict>"
	if [ -e "$FAKE/attached" ]; then
		echo "<dict><key>image-path</key><string>$IMG</string><key>image-alias</key><data>AAAA</data><key>system-entities</key><array><dict><key>dev-entry</key><string>/dev/disk9s2</string></dict><dict><key>dev-entry</key><string>/dev/disk9</string></dict></array></dict>"
	fi
	echo "</array></dict></plist>"
	;;
detach)
	if [ -e "$FAKE/detach-fail" ]; then echo "resource busy" >&2; exit 1; fi
	[ -e "$FAKE/detach-lies" ] || rm -f "$FAKE/attached"
	;;
*) exit 2 ;;
esac
`

// driveDirectFakeMount is mount on PATH.
const driveDirectFakeMount = `#!/bin/sh
if [ -e "$FAKE/mount-no-flags" ]; then
	echo "/dev/disk9s2 on /Volumes/scratch (hfs, local, journaled)"
else
	echo "/dev/disk3s1 on / (apfs, local)"
	echo "/dev/disk9s2 on /Volumes/scratch (hfs, local, nodev, nosuid, noexec, journaled, noowners, mounted by ana)"
fi
`

type driveDirectFixture struct {
	fake, state, image string
	m                  *macOSVMs
}

// newDriveDirectFixture puts the fake tools first on PATH and makes one drive's image. The real ps stays.
func newDriveDirectFixture(t *testing.T) *driveDirectFixture {
	t.Helper()
	bin, fake, state, root := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for name, script := range map[string]string{"hdiutil": driveDirectFakeHdiutil, "mount": driveDirectFakeMount} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &driveDirectFixture{fake: fake, state: state, m: &macOSVMs{layout: macvm.Layout{Root: root}, limitsFile: macOSVMLimitsFile(state)}}
	var err error
	if f.image, err = f.m.layout.DriveImage(driveDirectTestObject); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.image), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.image, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE", fake)
	t.Setenv("IMG", f.image)
	return f
}

func (f *driveDirectFixture) allow(t *testing.T) {
	t.Helper()
	driveDirectSettingSaver(f.state)(true)
	if !driveDirectAllowed(driveDirectSettingFile(f.m.limitsFile)) {
		t.Fatal("the saved setting reads off")
	}
}

func (f *driveDirectFixture) steer(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(f.fake, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *driveDirectFixture) unsteer(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.Remove(filepath.Join(f.fake, n)); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *driveDirectFixture) has(name string) bool {
	_, err := os.Stat(filepath.Join(f.fake, name))
	return err == nil
}

func (f *driveDirectFixture) calls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.fake, "calls"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

func (f *driveDirectFixture) args(in io.Reader, out io.Writer) driveDirectArgs {
	return driveDirectArgs{name: "scratch", in: in, out: out,
		find: func(name string) (string, error) {
			if name != "scratch" {
				return "", errors.New("no such drive")
			}
			return driveDirectTestObject, nil
		}}
}

func (f *driveDirectFixture) open(in io.Reader, out io.Writer) error {
	return f.m.driveDirect(context.Background(), &driveDirectFlags{direct: true}, f.args(in, out))
}

func (f *driveDirectFixture) recoverDrive(out io.Writer) error {
	return f.m.driveDirect(context.Background(), &driveDirectFlags{direct: true, recover: true}, f.args(strings.NewReader(""), out))
}

// wantRunRefused says a run can't lock the drive for the marker's sake, and that the image is still there.
func (f *driveDirectFixture) wantRunRefused(t *testing.T) {
	t.Helper()
	_, err := macvm.LockDisk(f.image)
	var mounted *macvm.DiskMountedError
	if !errors.As(err, &mounted) {
		t.Fatalf("LockDisk = %v, want the drive refused for its marker", err)
	}
}

func (f *driveDirectFixture) wantRunAllowed(t *testing.T) {
	t.Helper()
	unlock, err := macvm.LockDisk(f.image)
	if err != nil {
		t.Fatalf("LockDisk = %v, want the drive free", err)
	}
	unlock()
}

// The whole route: the exact flags, the marker on disk before hdiutil starts, the approved lines, and a verified detach.
func TestDriveDirectMountsWithTheFlagsAndDetachesOnReturn(t *testing.T) {
	f := newDriveDirectFixture(t)
	f.allow(t)
	var out bytes.Buffer
	if err := f.open(strings.NewReader("\n"), &out); err != nil {
		t.Fatal(err)
	}
	wantAttach := "attach -plist -nobrowse -noautoopen -imagekey diskimage-class=CRawDiskImage -mountoptions noexec,nosuid,nodev " + f.image + "\n"
	if !strings.Contains(f.calls(t), wantAttach) {
		t.Fatalf("hdiutil calls:\n%s\nwant %s", f.calls(t), wantAttach)
	}
	if !strings.Contains(f.calls(t), "detach /dev/disk9\n") {
		t.Fatalf("hdiutil calls:\n%s\nwant the whole disk detached", f.calls(t))
	}
	marker, err := os.ReadFile(filepath.Join(f.fake, "marker-at-attach"))
	if err != nil || !strings.Contains(string(marker), `"phase":"attaching"`) || !strings.Contains(string(marker), f.image) {
		t.Fatalf("marker when hdiutil started = %s, %v, want phase and identity", marker, err)
	}
	for _, want := range []string{
		`Mounted "scratch" at /Volumes/scratch with noexec, nosuid and nodev. This Mac's system reads the disk an agent wrote, so open it only if you trust that agent's work.`,
		"Files you write here use your Mac user's ownership and permissions. drive open scratch writes files back as the agent.",
		"This is logged on this Mac only. It leaves no row in your organisation's audit log.",
		"Press Return to detach the drive.",
	} {
		if !strings.Contains(out.String(), want+"\n") {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	log, err := os.ReadFile(filepath.Join(f.state, driveDirectLogName))
	if err != nil || !strings.Contains(string(log), `"event":"mount"`) || !strings.Contains(string(log), `"event":"detached"`) {
		t.Fatalf("local log = %s, %v", log, err)
	}
	f.wantRunAllowed(t)
}

// The lock is held while the drive is mounted, so a run or a second command gets the in-use answer.
func TestDriveDirectHoldsTheLockWhileMounted(t *testing.T) {
	f := newDriveDirectFixture(t)
	f.allow(t)
	in, press := io.Pipe()
	defer press.Close()
	lines, w := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- f.open(in, w); w.Close() }()
	seen := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		var all strings.Builder
		for {
			n, err := lines.Read(buf)
			all.Write(buf[:n])
			if strings.Contains(all.String(), "Press Return") {
				close(seen)
				_, _ = io.Copy(io.Discard, lines)
				return
			}
			if err != nil {
				close(seen)
				return
			}
		}
	}()
	<-seen
	if _, err := macvm.LockDisk(f.image); !errors.Is(err, macvm.ErrDiskInUse) {
		t.Fatalf("LockDisk while mounted = %v, want ErrDiskInUse", err)
	}
	if err := f.open(strings.NewReader("\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "is in another macOS VM") {
		t.Fatalf("a second open while mounted = %v, want the lock sentence", err)
	}
	if err := f.recoverDrive(io.Discard); err == nil || !strings.Contains(err.Error(), "is in another macOS VM") {
		t.Fatalf("recover while mounted = %v, want the lock sentence", err)
	}
	press.Write([]byte("\n"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.wantRunAllowed(t)
}

// Off, absent, unreadable and malformed all refuse, before any tool runs and without looking the drive up.
func TestDriveDirectRefusedUnlessTheSettingIsPlainlyOn(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, file string){
		"never heard": func(*testing.T, string) {},
		"unparsable": func(t *testing.T, file string) {
			if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"empty object": func(t *testing.T, file string) {
			if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"explicit false": func(t *testing.T, file string) {
			if err := os.WriteFile(file, []byte(`{"allow_direct_drive_mount":false}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"not a boolean": func(t *testing.T, file string) {
			if err := os.WriteFile(file, []byte(`{"allow_direct_drive_mount":"true"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable: a directory": func(t *testing.T, file string) {
			if err := os.Mkdir(file, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"turned off after on": func(t *testing.T, file string) {
			save := driveDirectSettingSaver(filepath.Dir(file))
			save(true)
			save(false)
			if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("off left the file: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDriveDirectFixture(t)
			setup(t, filepath.Join(f.state, driveDirectSettingName))
			a := f.args(strings.NewReader("\n"), io.Discard)
			a.find = func(string) (string, error) { t.Error("the drive was looked up"); return "", errors.New("no") }
			err := f.m.driveDirect(context.Background(), &driveDirectFlags{direct: true}, a)
			want := "Mounting a drive directly is off on this install. An admin can turn it on in Settings, under macOS virtual machines. To open the drive in a throwaway VM instead, run drive open scratch. The setting can't stop anyone who can run hdiutil on this Mac."
			if err == nil || err.Error() != want {
				t.Fatalf("refusal = %v", err)
			}
			if c := f.calls(t); c != "" {
				t.Fatalf("hdiutil ran: %s", c)
			}
			f.wantRunAllowed(t)
		})
	}
	t.Run("no state directory known", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		f.m.limitsFile = ""
		if err := f.open(strings.NewReader("\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "is off on this install") {
			t.Fatalf("refusal = %v", err)
		}
	})
}

// A marker left behind refuses a new mount too, and --recover without --direct is no route to anything.
func TestDriveDirectRefusedOverAMarker(t *testing.T) {
	f := newDriveDirectFixture(t)
	f.allow(t)
	if err := macvm.WriteDiskMount(f.image, macvm.DiskMount{Phase: macvm.DiskMountAttached, Name: "scratch", Image: f.image}); err != nil {
		t.Fatal(err)
	}
	var mounted *macvm.DiskMountedError
	if err := f.open(strings.NewReader("\n"), io.Discard); !errors.As(err, &mounted) {
		t.Fatalf("open over a marker = %v", err)
	}
	if c := f.calls(t); c != "" {
		t.Fatalf("hdiutil ran: %s", c)
	}
	err := f.m.driveDirect(context.Background(), &driveDirectFlags{recover: true}, f.args(strings.NewReader(""), io.Discard))
	if err == nil || !strings.Contains(err.Error(), "--recover goes with --direct") {
		t.Fatalf("--recover alone = %v", err)
	}
	f.wantRunRefused(t)
}

// Recovery works with the setting off: turning it off must not strand a drive.
func TestDriveDirectRecoverNeedsNoSettingAndClearsOnlyOnProof(t *testing.T) {
	marked := func(t *testing.T, f *driveDirectFixture) {
		t.Helper()
		if err := macvm.WriteDiskMount(f.image, macvm.DiskMount{Phase: macvm.DiskMountAttached, Name: "scratch", Image: f.image}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("not attached clears", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		marked(t, f)
		if err := f.recoverDrive(io.Discard); err != nil {
			t.Fatal(err)
		}
		f.wantRunAllowed(t)
	})
	t.Run("no marker has nothing to do", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		var out bytes.Buffer
		if err := f.recoverDrive(&out); err != nil || !strings.Contains(out.String(), "Nothing to clear") {
			t.Fatalf("%v, %s", err, out.String())
		}
		if c := f.calls(t); c != "" {
			t.Fatalf("hdiutil ran: %s", c)
		}
	})
	t.Run("a torn marker clears on the same proof", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		if err := os.WriteFile(f.image+".mounted", []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.recoverDrive(io.Discard); err != nil {
			t.Fatal(err)
		}
		f.wantRunAllowed(t)
	})
	t.Run("still attached is detached, said, and cleared", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		marked(t, f)
		f.steer(t, "attached")
		var out bytes.Buffer
		if err := f.recoverDrive(&out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Detaching /dev/disk9.") || !strings.Contains(f.calls(t), "detach /dev/disk9\n") {
			t.Fatalf("output %s; calls %s", out.String(), f.calls(t))
		}
		f.wantRunAllowed(t)
	})
	for name, tc := range map[string]struct {
		steer []string
		want  string
	}{
		"failed detach":              {[]string{"attached", "detach-fail"}, "resource busy"},
		"detach that left it there":  {[]string{"attached", "detach-lies"}, "still attached"},
		"hdiutil info fails":         {[]string{"info-fail"}, "info failed"},
		"hdiutil info is unparsable": {[]string{"info-garbage"}, "not a property list"},
	} {
		t.Run(name+" keeps the marker", func(t *testing.T) {
			f := newDriveDirectFixture(t)
			marked(t, f)
			f.steer(t, tc.steer...)
			err := f.recoverDrive(io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "the marker stays") {
				t.Fatalf("recover = %v, want %q", err, tc.want)
			}
			f.wantRunRefused(t)
		})
	}
	t.Run("a marker naming another image keeps it", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		if err := macvm.WriteDiskMount(f.image, macvm.DiskMount{Phase: macvm.DiskMountAttached, Name: "scratch", Image: "/other/image.img"}); err != nil {
			t.Fatal(err)
		}
		if err := f.recoverDrive(io.Discard); err == nil || !strings.Contains(err.Error(), "the marker stays") {
			t.Fatalf("recover = %v", err)
		}
		f.wantRunRefused(t)
	})
	t.Run("ps that fails or lists nothing keeps the marker", func(t *testing.T) {
		for _, script := range []string{"#!/bin/sh\nexit 1\n", "#!/bin/sh\nexit 0\n"} {
			f := newDriveDirectFixture(t)
			marked(t, f)
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			if err := f.recoverDrive(io.Discard); err == nil || !strings.Contains(err.Error(), "the marker stays") {
				t.Fatalf("recover = %v", err)
			}
			f.wantRunRefused(t)
		}
	})
}

// A failed attach proves itself detached and clears; one whose state can't be proved keeps the marker.
func TestDriveDirectFailedAttach(t *testing.T) {
	t.Run("hdiutil refuses: nothing attached, marker gone", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		f.allow(t)
		f.steer(t, "attach-fail")
		if err := f.open(strings.NewReader("\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "attach failed") {
			t.Fatalf("open = %v", err)
		}
		f.wantRunAllowed(t)
	})
	t.Run("attach output unparsable: found attached, detached, marker gone", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		f.allow(t)
		f.steer(t, "attach-garbage")
		if err := f.open(strings.NewReader("\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "not a property list") {
			t.Fatalf("open = %v", err)
		}
		if f.has("attached") {
			t.Fatal("left attached")
		}
		f.wantRunAllowed(t)
	})
	t.Run("the flags did not take: detached and refused", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		f.allow(t)
		f.steer(t, "mount-no-flags")
		var out bytes.Buffer
		if err := f.open(strings.NewReader("\n"), &out); err == nil || !strings.Contains(err.Error(), "is not mounted with noexec, nosuid, nodev") {
			t.Fatalf("open = %v", err)
		}
		if f.has("attached") || strings.Contains(out.String(), "Mounted ") {
			t.Fatalf("attached=%v, said %s", f.has("attached"), out.String())
		}
		f.wantRunAllowed(t)
	})
	t.Run("detach fails after the flags did not take: marker stays", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		f.allow(t)
		f.steer(t, "mount-no-flags", "detach-fail")
		var mounted *macvm.DiskMountedError
		if err := f.open(strings.NewReader("\n"), io.Discard); !errors.As(err, &mounted) {
			t.Fatalf("open = %v", err)
		}
		f.wantRunRefused(t)
	})
	t.Run("an attach that outlasts its wait stays uncertain", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		f.allow(t)
		f.steer(t, "attach-wait")
		old := driveDirectAttachWait
		driveDirectAttachWait = 300 * time.Millisecond
		t.Cleanup(func() { driveDirectAttachWait = old })
		var mounted *macvm.DiskMountedError
		err := f.open(strings.NewReader("\n"), io.Discard)
		f.unsteer(t, "attach-wait")
		if !errors.As(err, &mounted) || !strings.Contains(err.Error(), "did not finish") {
			t.Fatalf("open = %v", err)
		}
		f.wantRunRefused(t)
	})
	t.Run("a detach that fails at the close keeps the marker and names recovery", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		f.allow(t)
		f.steer(t, "detach-fail")
		err := f.open(strings.NewReader("\n"), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "drive open --direct --recover scratch") {
			t.Fatalf("open = %v", err)
		}
		f.wantRunRefused(t)
		f.unsteer(t, "detach-fail")
		if err := f.recoverDrive(io.Discard); err != nil {
			t.Fatal(err)
		}
		f.wantRunAllowed(t)
	})
}

// A volume's name is the agent's: control characters in a mount point never reach the terminal raw.
func TestDriveDirectCleanQuotesWhatIsNotPrintable(t *testing.T) {
	if got := driveDirectClean("/Volumes/My Drive é"); got != "/Volumes/My Drive é" {
		t.Errorf("clean = %q", got)
	}
	if got := driveDirectClean("/Volumes/x\x1b]0;owned\x07"); strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("clean = %q", got)
	}
	if got := driveDirectClean("/Volumes/a\u202eb"); strings.ContainsRune(got, '\u202e') {
		t.Errorf("clean = %q", got)
	}
}

// Only the line of that mount point counts, and a volume named to look like it has the flags doesn't pass.
func TestDriveDirectFlagsHeld(t *testing.T) {
	good := "/dev/disk9s2 on /Volumes/a (hfs, nodev, nosuid, noexec)\n/dev/disk8 on /Volumes/b (hfs, local)\n"
	if !driveDirectFlagsHeld(good, "/Volumes/a") {
		t.Error("good line refused")
	}
	if driveDirectFlagsHeld(good, "/Volumes/b") || driveDirectFlagsHeld(good, "/Volumes/c") {
		t.Error("bad or missing line passed")
	}
	sly := "/Volumes/x (nodev, nosuid, noexec)"
	if driveDirectFlagsHeld("/dev/disk9s2 on "+sly+" (hfs, local)\n", sly) {
		t.Error("flags in the volume's name passed")
	}
}

// hdiutil's plist: nothing attached is an empty list, other images don't count, data elements are skipped,
// and a listing with no images key or an image with no path is an error, not a no.
func TestDriveDirectAttachedParsing(t *testing.T) {
	f := newDriveDirectFixture(t)
	if devs, err := driveDirectAttached(f.image); err != nil || len(devs) != 0 {
		t.Fatalf("none attached = %v, %v", devs, err)
	}
	f.steer(t, "attached")
	if devs, err := driveDirectAttached(f.image); err != nil || fmt.Sprint(devs) != "[/dev/disk9s2 /dev/disk9]" {
		t.Fatalf("attached = %v, %v", devs, err)
	}
	for _, body := range []string{
		"<plist><dict><key>framework</key><string>1</string></dict></plist>",
		"<plist><dict><key>images</key><array><dict><key>system-entities</key><array/></dict></array></dict></plist>",
		"<plist><array/></plist>",
		"",
	} {
		bin := t.TempDir()
		script := "#!/bin/sh\ncat <<'END'\n" + body + "\nEND\n"
		if err := os.WriteFile(filepath.Join(bin, "hdiutil"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
		if devs, err := driveDirectAttached(f.image); err == nil {
			t.Errorf("%q read as attached=%v with no error", body, devs)
		}
	}
	if _, err := driveDirectParsePlist([]byte("<plist><dict><key>images")); err == nil {
		t.Fatal("output cut short read as a property list")
	}
}

// The merge seam: the two flags parse, and only one of them chooses this route.
func TestDriveDirectFlagsChooseTheRoute(t *testing.T) {
	cmd := &cobra.Command{Use: "open"}
	f := addDriveDirectFlags(cmd)
	if f.wanted() {
		t.Fatal("no flags chose this route")
	}
	if err := cmd.ParseFlags([]string{"--direct", "--recover"}); err != nil {
		t.Fatal(err)
	}
	if !f.direct || !f.recover || !f.wanted() {
		t.Fatalf("flags = %+v", f)
	}
}

// TestDriveDirectChildCommand is not a test: run with the env set, it is the `drive open --direct` the
// kill and signal tests below end. Its input never ends.
func TestDriveDirectChildCommand(t *testing.T) {
	if os.Getenv(driveDirectTestChildEnv) == "" {
		t.Skip("the child of the kill tests")
	}
	in, _ := io.Pipe()
	state := os.Getenv("STATE")
	m := &macOSVMs{layout: macvm.Layout{Root: os.Getenv("ROOT")}, limitsFile: macOSVMLimitsFile(state)}
	a := driveDirectArgs{name: "scratch", in: in, out: os.Stdout, find: func(string) (string, error) { return driveDirectTestObject, nil }}
	if err := m.driveDirect(context.Background(), &driveDirectFlags{direct: true}, a); err != nil {
		fmt.Println("child:", err)
		os.Exit(3)
	}
	fmt.Println("child: clean exit")
}

// startChild runs the command in another process, in its own group so a test's signal reaches only it.
func (f *driveDirectFixture) startChild(t *testing.T) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestDriveDirectChildCommand$", "-test.count=1")
	cmd.Env = append(os.Environ(), driveDirectTestChildEnv+"=1", "STATE="+f.state, "ROOT="+f.m.layout.Root)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, &out
}

func driveDirectWaitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A CLI killed while mounted loses its lock and leaves the mount: a run is refused, though nothing holds the
// flock, until --recover detaches the image and clears the marker.
func TestDriveDirectKilledWhileMountedLeavesARunRefusedUntilRecover(t *testing.T) {
	f := newDriveDirectFixture(t)
	f.allow(t)
	child, _ := f.startChild(t)
	driveDirectWaitFor(t, "the attach", func() bool {
		m, found, _ := macvm.ReadDiskMount(f.image)
		return found && m.Phase == macvm.DiskMountAttached
	})
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	f.wantRunRefused(t)
	if !f.has("attached") {
		t.Fatal("the fake lost its attach")
	}
	if err := f.recoverDrive(io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.has("attached") {
		t.Fatal("recover left the image attached")
	}
	f.wantRunAllowed(t)
}

// A CLI killed before its attach finished leaves hdiutil running, detached from it. `hdiutil info` says not
// attached, which proves nothing yet: recovery refuses while that hdiutil lives. When it finishes late, the
// image is attached, and only now can recovery detach it and clear the marker.
func TestDriveDirectKilledDuringAttachRefusesRecoveryUntilTheLateChildEnds(t *testing.T) {
	f := newDriveDirectFixture(t)
	f.allow(t)
	f.steer(t, "attach-wait")
	child, _ := f.startChild(t)
	driveDirectWaitFor(t, "hdiutil to start", func() bool { return strings.Contains(f.calls(t), "attach ") })
	if m, found, err := macvm.ReadDiskMount(f.image); !found || err != nil || m.Phase != macvm.DiskMountAttaching {
		t.Fatalf("marker = %+v, %v, %v", m, found, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	f.wantRunRefused(t)
	if devs, err := driveDirectAttached(f.image); err != nil || len(devs) != 0 {
		t.Fatalf("hdiutil info = %v, %v, want not attached yet", devs, err)
	}
	err := f.recoverDrive(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "still working on this image") {
		t.Fatalf("recover with the attach pending = %v", err)
	}
	f.wantRunRefused(t)
	f.unsteer(t, "attach-wait") // the late child finishes
	driveDirectWaitFor(t, "the late attach", func() bool { return f.has("attached") })
	driveDirectWaitFor(t, "hdiutil to end", func() bool { return driveDirectNoLiveTool(f.image) == nil })
	f.wantRunRefused(t)
	if err := f.recoverDrive(io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.has("attached") {
		t.Fatal("recover left the late attach attached")
	}
	f.wantRunAllowed(t)
}

// Each signal a closing terminal or a person sends detaches and clears, as Return does. A signal that lands
// during the attach waits for it, so the attach it let finish is detached too.
func TestDriveDirectSignalsDetach(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			f := newDriveDirectFixture(t)
			f.allow(t)
			child, out := f.startChild(t)
			driveDirectWaitFor(t, "the attach", func() bool {
				m, found, _ := macvm.ReadDiskMount(f.image)
				return found && m.Phase == macvm.DiskMountAttached
			})
			if err := child.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err != nil {
				t.Fatalf("child: %v\n%s", err, out.String())
			}
			if f.has("attached") {
				t.Fatalf("still attached\n%s", out.String())
			}
			f.wantRunAllowed(t)
		})
	}
	t.Run("during the attach", func(t *testing.T) {
		f := newDriveDirectFixture(t)
		f.allow(t)
		f.steer(t, "attach-wait")
		child, out := f.startChild(t)
		driveDirectWaitFor(t, "hdiutil to start", func() bool { return strings.Contains(f.calls(t), "attach ") })
		if err := child.Process.Signal(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		f.unsteer(t, "attach-wait")
		if err := child.Wait(); err != nil {
			t.Fatalf("child: %v\n%s", err, out.String())
		}
		if f.has("attached") {
			t.Fatalf("still attached\n%s", out.String())
		}
		f.wantRunAllowed(t)
	})
}
