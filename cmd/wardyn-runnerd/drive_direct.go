// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// `drive open --direct <name>` mounts a drive on the Mac itself, so this Mac's kernel parses a disk an agent
// wrote. It is off unless the organisation's macos_vm.allow_direct_drive_mount is on, and it can't stop a
// person who runs hdiutil themselves. The disk's lock is held while the drive is mounted; a killed command
// loses the lock and leaves the mount, so a marker (macvm.WriteDiskMount) keeps refusing runs until
// `--recover` has proved the image detached with no hdiutil still working on it.

const (
	// driveDirectSettingName is the file in the runner's state directory that says the organisation allows
	// a direct mount. Only the file holding an explicit true allows it.
	driveDirectSettingName = "macos-vm-direct-mount.json"
	// driveDirectLogName is the local record of direct mounts, in the runner's state directory.
	driveDirectLogName = "drive-direct.log"

	driveDirectToolWait = time.Minute
)

// driveDirectAttachWait bounds hdiutil attach. One that outlasts it is left uncertain: the marker stays.
var driveDirectAttachWait = 5 * time.Minute

// driveDirectMountFlags are what attach asks for, and what `mount` must then show on every volume.
var driveDirectMountFlags = []string{"noexec", "nosuid", "nodev"}

// driveDirectFlags is `drive open`'s two flags of this route.
type driveDirectFlags struct{ direct, recover bool }

// addDriveDirectFlags gives `drive open` the flags of this route.
func addDriveDirectFlags(open *cobra.Command) *driveDirectFlags {
	f := &driveDirectFlags{}
	open.Flags().BoolVar(&f.direct, "direct", false, "mount the drive on this Mac, if an admin allows it, instead of opening it in a throwaway VM")
	open.Flags().BoolVar(&f.recover, "recover", false, "with --direct: check that a drive left mounted is detached, and clear its marker")
	return f
}

// wanted says this `drive open` belongs to this route.
func (f *driveDirectFlags) wanted() bool { return f.direct || f.recover }

// driveDirectArgs is one `drive open --direct`. find turns the name a person typed into the drive's object name.
type driveDirectArgs struct {
	name string
	find func(name string) (object string, err error)
	in   io.Reader
	out  io.Writer
}

// driveDirect is `drive open --direct`. The setting is read first, so a refusal says nothing about the drive.
// Recovery needs no setting: turning the setting off must not strand a drive.
func (m *macOSVMs) driveDirect(ctx context.Context, f *driveDirectFlags, a driveDirectArgs) error {
	if !f.direct {
		return errors.New("--recover goes with --direct: drive open --direct --recover " + a.name)
	}
	if !f.recover && !driveDirectAllowed(driveDirectSettingFile(m.limitsFile)) {
		return fmt.Errorf("Mounting a drive directly is off on this install. An admin can turn it on in Settings, under macOS virtual machines. To open the drive in a throwaway VM instead, run drive open %s. The setting can't stop anyone who can run hdiutil on this Mac.", a.name)
	}
	object, err := a.find(a.name)
	if err != nil {
		return err
	}
	image, err := m.layout.DriveImage(object)
	if err != nil {
		return err
	}
	unlock, err := macvm.LockDiskForMount(image)
	if errors.Is(err, macvm.ErrDiskInUse) {
		return errors.New(runner.MacVMDiskLocked(a.name))
	}
	if err != nil {
		return err
	}
	defer unlock()
	logFile := driveDirectLogFile(m.limitsFile)
	if f.recover {
		return driveDirectRecover(image, a, logFile)
	}
	return driveDirectMount(ctx, image, a, logFile)
}

// driveDirectMount attaches the image, waits for Return or a signal, and detaches it. The caller holds the disk's lock.
func driveDirectMount(ctx context.Context, image string, a driveDirectArgs, logFile string) error {
	if fi, err := os.Lstat(image); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("the drive %q has no disk image on this Mac", a.name)
	}
	if _, found, _ := macvm.ReadDiskMount(image); found {
		return &macvm.DiskMountedError{Name: a.name}
	}
	// From here a signal waits its turn: it must not cut an attach or a detach short.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stop)
	if err := driveDirectLog(logFile, "mount", a.name, image); err != nil {
		return fmt.Errorf("the direct mount was not started: its local log can't be written: %w", err)
	}
	if err := macvm.WriteDiskMount(image, macvm.DiskMount{Phase: macvm.DiskMountAttaching, Name: a.name, Image: image}); err != nil {
		return fmt.Errorf("the direct mount was not started: %w", err)
	}
	points, err := driveDirectAttach(image)
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(fmt.Errorf("hdiutil attach did not finish in %s", driveDirectAttachWait), &macvm.DiskMountedError{Name: a.name})
	}
	if err == nil {
		err = macvm.WriteDiskMount(image, macvm.DiskMount{Phase: macvm.DiskMountAttached, Name: a.name, Image: image})
	}
	if err != nil {
		if serr := driveDirectDetachClear(image, a.out); serr != nil {
			return errors.Join(err, serr, &macvm.DiskMountedError{Name: a.name})
		}
		_ = driveDirectLog(logFile, "detached", a.name, image)
		return err
	}
	where := make([]string, len(points))
	for i, p := range points {
		where[i] = driveDirectClean(p)
	}
	fmt.Fprintf(a.out, "Mounted %q at %s with noexec, nosuid and nodev. This Mac's system reads the disk an agent wrote, so open it only if you trust that agent's work.\n", a.name, strings.Join(where, ", "))
	fmt.Fprintf(a.out, "Files you write here use your Mac user's ownership and permissions. drive open %s writes files back as the agent.\n", a.name)
	fmt.Fprintln(a.out, "This is logged on this Mac only. It leaves no row in your organisation's audit log.")
	fmt.Fprintln(a.out, "Press Return to detach the drive.")

	pressed := make(chan struct{})
	go func() {
		_, _ = bufio.NewReader(a.in).ReadString('\n') // an input that ends counts as Return
		close(pressed)
	}()
	select {
	case <-pressed:
	case <-stop:
	case <-ctx.Done():
	}
	if err := driveDirectDetachClear(image, a.out); err != nil {
		return errors.Join(err, &macvm.DiskMountedError{Name: a.name})
	}
	if err := driveDirectLog(logFile, "detached", a.name, image); err != nil {
		slog.Warn("drive open --direct: the local log was not written", "error", err.Error())
	}
	fmt.Fprintf(a.out, "Detached %q.\n", a.name)
	return nil
}

// driveDirectRecover is `--recover`: a person's act, under the disk's lock. An error keeps the marker.
func driveDirectRecover(image string, a driveDirectArgs, logFile string) error {
	mark, found, rerr := macvm.ReadDiskMount(image)
	if !found {
		fmt.Fprintf(a.out, "The drive %q has no mount marker on this Mac. Nothing to clear.\n", a.name)
		return nil
	}
	// A marker that can't be read says nothing, so the image's own path is what's checked. One that names
	// another image is not this drive's to clear.
	if rerr == nil && mark.Image != image {
		return fmt.Errorf("the marker stays: it names the image %s, not %s", driveDirectClean(mark.Image), image)
	}
	if err := driveDirectDetachClear(image, a.out); err != nil {
		return fmt.Errorf("the marker stays, so runs on %q stay refused: %w", a.name, err)
	}
	if err := driveDirectLog(logFile, "recovered", a.name, image); err != nil {
		slog.Warn("drive open --direct --recover: the local log was not written", "error", err.Error())
	}
	fmt.Fprintf(a.out, "The image of %q is not attached and no hdiutil is working on it. Cleared its marker, so runs can use it again.\n", a.name)
	return nil
}

// driveDirectDetachClear proves the image detached and no hdiutil at work on it, detaching it if it is still
// attached, and only then removes the marker. Anything uncertain is an error and leaves the marker: an hdiutil
// that can't be read, an output that can't be parsed, a detach that failed, an attach still running.
func driveDirectDetachClear(image string, out io.Writer) error {
	if err := driveDirectNoLiveTool(image); err != nil {
		return err
	}
	devs, err := driveDirectAttached(image)
	if err != nil {
		return err
	}
	if len(devs) > 0 {
		dev := slices.MinFunc(devs, func(a, b string) int { return len(a) - len(b) }) // the whole disk
		fmt.Fprintf(out, "Detaching %s.\n", driveDirectClean(dev))
		if _, err := driveDirectRun(driveDirectToolWait, "hdiutil", "detach", dev); err != nil {
			return fmt.Errorf("hdiutil could not detach %s: %w", driveDirectClean(dev), err)
		}
		if devs, err = driveDirectAttached(image); err != nil {
			return err
		}
		if len(devs) > 0 {
			return fmt.Errorf("the image is still attached as %s after hdiutil detached it", driveDirectClean(strings.Join(devs, ", ")))
		}
	}
	if err := driveDirectNoLiveTool(image); err != nil { // an attach that began between the reads
		return err
	}
	return macvm.ClearDiskMount(image)
}

// driveDirectAttach attaches and mounts the image, and proves the flags on every volume it mounted. An error
// but for an attach that timed out (context.DeadlineExceeded) means hdiutil has finished: the caller detaches.
func driveDirectAttach(image string) (points []string, err error) {
	out, err := driveDirectRun(driveDirectAttachWait, "hdiutil", "attach", "-plist", "-nobrowse", "-noautoopen",
		"-imagekey", "diskimage-class=CRawDiskImage", "-mountoptions", strings.Join(driveDirectMountFlags, ","), image)
	if err != nil {
		return nil, err
	}
	plist, err := driveDirectParsePlist(out)
	if err != nil {
		return nil, fmt.Errorf("hdiutil attach: %w", err)
	}
	if points = driveDirectEntryStrings(plist["system-entities"], "mount-point"); len(points) == 0 {
		return nil, errors.New("hdiutil attached the image but mounted no volume from it")
	}
	table, err := driveDirectRun(driveDirectToolWait, "mount")
	if err != nil {
		return nil, fmt.Errorf("mount: %w", err)
	}
	for _, p := range points {
		if !driveDirectFlagsHeld(string(table), p) {
			return nil, fmt.Errorf("%s is not mounted with %s, so it was detached", driveDirectClean(p), strings.Join(driveDirectMountFlags, ", "))
		}
	}
	return points, nil
}

// driveDirectFlagsHeld reads the `mount` output's line for the mount point: `<dev> on <point> (<flags>)`.
func driveDirectFlagsHeld(table, point string) bool {
	needle := " on " + point + " ("
	for line := range strings.SplitSeq(table, "\n") {
		_, after, ok := strings.Cut(line, needle)
		if !ok {
			continue
		}
		flags := strings.Split(strings.TrimSuffix(strings.TrimSpace(after), ")"), ",")
		for i := range flags {
			flags[i] = strings.TrimSpace(flags[i])
		}
		return !slices.ContainsFunc(driveDirectMountFlags, func(want string) bool { return !slices.Contains(flags, want) })
	}
	return false
}

// driveDirectAttached is the device entries `hdiutil info` lists for the image: none means it isn't attached.
func driveDirectAttached(image string) ([]string, error) {
	out, err := driveDirectRun(driveDirectToolWait, "hdiutil", "info", "-plist")
	if err != nil {
		return nil, fmt.Errorf("hdiutil info: %w", err)
	}
	plist, err := driveDirectParsePlist(out)
	if err != nil {
		return nil, fmt.Errorf("hdiutil info: %w", err)
	}
	images, ok := plist["images"].([]any)
	if !ok {
		return nil, errors.New("hdiutil info: its output names no images list")
	}
	var devs []string
	for _, entry := range images {
		e, _ := entry.(map[string]any)
		path, _ := e["image-path"].(string)
		if path == "" {
			return nil, errors.New("hdiutil info: an image has no path")
		}
		if !driveDirectSameFile(path, image) {
			continue
		}
		found := driveDirectEntryStrings(e["system-entities"], "dev-entry")
		if len(found) == 0 {
			return nil, errors.New("hdiutil info: the image is listed with no device")
		}
		devs = append(devs, found...)
	}
	return devs, nil
}

// driveDirectSameFile compares hdiutil's path with the image's: hdiutil may name it through /private.
func driveDirectSameFile(path, image string) bool {
	if path == image {
		return true
	}
	a, aerr := os.Stat(path)
	b, berr := os.Stat(image)
	return aerr == nil && berr == nil && os.SameFile(a, b)
}

// driveDirectEntryStrings is key's value in each dictionary of the list, those that have one.
func driveDirectEntryStrings(list any, key string) []string {
	entries, _ := list.([]any)
	var found []string
	for _, entry := range entries {
		e, _ := entry.(map[string]any)
		if v, _ := e[key].(string); v != "" {
			found = append(found, v)
		}
	}
	return found
}

// driveDirectNoLiveTool is nil only when ps ran and lists no hdiutil naming the image: an hdiutil outlives a
// killed command, so a negative `hdiutil info` alone doesn't prove an attach won't finish later.
func driveDirectNoLiveTool(image string) error {
	out, err := driveDirectRun(driveDirectToolWait, "ps", "-axww", "-o", "pid=", "-o", "command=")
	if err != nil {
		return fmt.Errorf("ps: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return errors.New("ps listed no processes")
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if !strings.Contains(line, image) {
			continue
		}
		if slices.ContainsFunc(strings.Fields(line), func(word string) bool { return filepath.Base(word) == "hdiutil" }) {
			return fmt.Errorf("an hdiutil is still working on this image (%s). Wait for it to end, then try again", driveDirectClean(strings.TrimSpace(line)))
		}
	}
	return nil
}

// driveDirectRun runs a tool in its own session, so the Ctrl-C meant for this command can't cut it short,
// with no input, so it can't stop to ask for a password. It answers what the tool wrote to stdout. A tool
// that outlasts wait is killed and the error is context.DeadlineExceeded.
func driveDirectRun(wait time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, context.DeadlineExceeded
	}
	if err != nil {
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return nil, fmt.Errorf("%w: %s", err, driveDirectClean(text))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// driveDirectClean makes a string fit for the person's terminal. A volume's name is the agent's to choose
// and becomes its mount point, so anything but printing characters is shown quoted, never sent raw.
func driveDirectClean(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return s
	}
	return strconv.QuoteToGraphic(s)
}

// driveDirectParsePlist reads the XML property list hdiutil writes: its top dictionary, with dictionaries,
// arrays and strings kept and other values nil. Output cut short or not a property list is an error.
func driveDirectParsePlist(data []byte) (map[string]any, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("its output is not a property list: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local == "plist" {
			continue
		}
		v, err := driveDirectPlistValue(d, start)
		if err != nil {
			return nil, fmt.Errorf("its output is not a property list: %w", err)
		}
		top, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("its output is not a property list dictionary")
		}
		return top, nil
	}
}

func driveDirectPlistValue(d *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "string":
		var s string
		err := d.DecodeElement(&s, &start)
		return s, err
	case "array":
		list := []any{}
		err := driveDirectPlistChildren(d, func(child xml.StartElement) error {
			v, err := driveDirectPlistValue(d, child)
			list = append(list, v)
			return err
		})
		return list, err
	case "dict":
		dict := map[string]any{}
		key := ""
		err := driveDirectPlistChildren(d, func(child xml.StartElement) error {
			if child.Name.Local == "key" {
				return d.DecodeElement(&key, &child)
			}
			v, err := driveDirectPlistValue(d, child)
			dict[key] = v
			return err
		})
		return dict, err
	}
	return nil, d.Skip()
}

// driveDirectPlistChildren hands each child of the element just opened to visit, up to its end.
func driveDirectPlistChildren(d *xml.Decoder, visit func(xml.StartElement) error) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := visit(t); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

// driveDirectSettingFile and driveDirectLogFile sit beside limitsFile, the one path a macOSVMs keeps in the
// runner's state directory. No state directory known: "", which reads refused.
func driveDirectSettingFile(limitsFile string) string {
	if limitsFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(limitsFile), driveDirectSettingName)
}

func driveDirectLogFile(limitsFile string) string {
	if limitsFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(limitsFile), driveDirectLogName)
}

// driveDirectAllowed reads the saved setting and fails closed: absent, unreadable, unparsable, or anything
// but an explicit true refuses.
func driveDirectAllowed(file string) bool {
	if file == "" {
		return false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var saved struct {
		Allow *bool `json:"allow_direct_drive_mount"`
	}
	return json.Unmarshal(data, &saved) == nil && saved.Allow != nil && *saved.Allow
}

// driveDirectSettingSaver is runnerwire.RunnerHooks.DirectDriveMount: on every connect it saves what the
// organisation said, for the `drive open` that runs in its own process and never sees the connect. Off is the
// file's absence. A failed write leaves no file; a failed removal is logged and leaves the last answer.
func driveDirectSettingSaver(stateDir string) func(allowed bool) {
	file := filepath.Join(stateDir, driveDirectSettingName)
	return func(allowed bool) {
		var err error
		if allowed {
			err = driveDirectWriteSetting(file)
		} else if err = os.Remove(file); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		if err != nil {
			slog.Warn("drive open --direct: the organisation's setting was not saved", "allowed", allowed, "error", err.Error())
		}
	}
}

// driveDirectWriteSetting replaces the file whole: a temporary file in the same directory, then a rename.
func driveDirectWriteSetting(file string) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), driveDirectSettingName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(`{"allow_direct_drive_mount":true}`)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// driveDirectLog appends one line to this Mac's own record. Nothing of it leaves the Mac.
func driveDirectLog(file, event, name, image string) error {
	if file == "" {
		return errors.New("no state directory")
	}
	line, err := json.Marshal(map[string]string{"time": time.Now().UTC().Format(time.RFC3339), "event": event, "name": name, "image": image})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
