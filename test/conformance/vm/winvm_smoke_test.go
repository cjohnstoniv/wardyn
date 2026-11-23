// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && vm

package vm

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// TestWinVMSmoke is the Windows VM sandbox's blank-disk smoke test: no Windows is booted. A blank qcow2
// (made by the QEMU image's own qemu-img) is wrapped in a containerDisk image by docker import, and
// the real Docker driver starts it with WinArgs in the unprivileged QEMU container: the UEFI firmware
// draws on the display, QMP shows the swtpm sidecar's TPM, the desktop completes an RFB handshake, the
// NIC's stream is connected to the relay, a disk naming a backing file is refused, and teardown leaves
// no container or directory.
func TestWinVMSmoke(t *testing.T) {
	qemuImage, proxyImage := os.Getenv("WARDYN_TEST_VM_QEMU_IMAGE"), os.Getenv("WARDYN_TEST_VM_PROXY_IMAGE")
	if qemuImage == "" || proxyImage == "" {
		t.Skip("WARDYN_TEST_VM_QEMU_IMAGE and WARDYN_TEST_VM_PROXY_IMAGE not set")
	}
	ctx := context.Background()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	upNet := "wardyn-winconf-up-" + uuid.NewString()[:8]
	if _, err := cli.NetworkCreate(ctx, upNet, dockerclient.NetworkCreateOptions{Driver: "bridge", Internal: true,
		IPAM: &network.IPAM{Config: []network.IPAMConfig{{Subnet: netip.MustParsePrefix("11.231.3.0/24")}}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), upNet, dockerclient.NetworkRemoveOptions{}) })
	d, err := docker.New(docker.Config{ProxyImage: proxyImage, InternalNetwork: upNet})
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("", "wwv") // short: unix socket paths are bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	vc, err := d.ProbeVM(ctx, docker.VMConfig{StateDir: state, QEMUImage: qemuImage, CPUModel: cmp.Or(os.Getenv("WARDYN_TEST_VM_CPU_MODEL"), "Haswell-v4")})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := d.EnableVM(&vc); err != nil {
		t.Fatal(err)
	}
	wc, err := d.ProbeWindowsVM(ctx)
	if err != nil {
		t.Fatalf("Windows probe: %v", err)
	}
	if err := d.EnableWindowsVM(&wc); err != nil {
		t.Fatal(err)
	}

	blank := diskImage(t, cli, qemuImage, "qemu-img create -q -f qcow2 /out/d.qcow2 64M")
	backed := diskImage(t, cli, qemuImage, "qemu-img create -q -f qcow2 -F raw -b /etc/hostname -u /out/d.qcow2 64M")
	spec := func(image string) runner.SandboxSpec {
		return runner.SandboxSpec{RunID: uuid.New(), Image: image, MachineKind: types.MachineWindowsVM, ConfinementClass: types.CC1, Interactive: true,
			ProxyConfig: runner.ProxyConfig{RunToken: "tok", ControlPlaneURL: "http://127.0.0.1:8080", Policy: types.RunPolicySpec{
				FirstUseApproval: types.FirstUseAlwaysDeny, MinConfinementClass: types.CC1}},
			Resources: runner.Resources{CPUMillis: 2000, MemoryMiB: 4096}}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// A disk that names a backing file is refused before QEMU starts.
	bad := spec(backed)
	if _, err := d.CreateSandbox(cctx, bad); err == nil || !strings.Contains(err.Error(), "backing file") {
		t.Fatalf("a backing-file disk = %v, want a refusal naming it", err)
	}
	noResidue(t, cli, state, bad.RunID)

	good := spec(blank)
	sb, err := d.CreateSandbox(cctx, good)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	torn := false
	t.Cleanup(func() {
		if !torn {
			_ = d.KillSandbox(context.Background(), sb.Ref)
		}
	})
	if sb.EnforcedClass != types.CC3 || sb.Machine == nil || sb.Machine.Accelerator != "kvm" {
		t.Fatalf("sandbox %+v machine %+v", sb, sb.Machine)
	}
	ctl := filepath.Join(state, "runs", good.RunID.String(), "ctl")

	// The TPM is the swtpm sidecar's, through the emulator backend.
	var tpms []struct {
		Model   string `json:"model"`
		Options struct {
			Type string `json:"type"`
		} `json:"options"`
	}
	qmp(t, ctl, "query-tpm", nil, &tpms)
	if len(tpms) != 1 || tpms[0].Model != "tpm-crb" || tpms[0].Options.Type != "emulator" {
		t.Fatalf("query-tpm = %+v, want one emulated TPM 2.0 (CRB)", tpms)
	}
	// The NIC's stream is connected to the relay's socket.
	var network string
	qmp(t, ctl, "human-monitor-command", map[string]string{"command-line": "info network"}, &network)
	if !strings.Contains(network, "e1000e") || !strings.Contains(network, qemu.NetSocket) {
		t.Fatalf("info network = %q, want the e1000e NIC on the relay's stream", network)
	}
	// The desktop: an RFB handshake through the driver, and the firmware drawing on it within a minute.
	desk, err := d.DialDesktop(ctx, sb.Ref)
	if err != nil {
		t.Fatal(err)
	}
	drawn, err := rfbDrawn(desk, time.Minute)
	_ = desk.Close()
	if err != nil || !drawn {
		t.Fatalf("the desktop: drawn %v, %v: the UEFI firmware never drew on the display", drawn, err)
	}
	var status struct {
		Status string `json:"status"`
	}
	qmp(t, ctl, "query-status", nil, &status)
	if status.Status != "running" {
		t.Fatalf("the VM is %q, want running", status.Status)
	}

	if err := d.StopSandbox(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
	torn = true
	noResidue(t, cli, state, good.RunID)
	// The orphan sweep finds nothing of either run left to clean.
	if n, err := d.SweepOrphanedSandboxes(ctx, 0, func(uuid.UUID) bool { return true }); err != nil || n != 0 {
		t.Fatalf("the orphan sweep after teardown cleaned %d runs (%v), want none", n, err)
	}
}

// noResidue fails when a container or the run directory of runID is left.
func noResidue(t *testing.T, cli *dockerclient.Client, state string, runID uuid.UUID) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(state, "runs", runID.String())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run directory of %s survived: %v", runID, err)
	}
	list, err := cli.ContainerList(context.Background(), dockerclient.ContainerListOptions{All: true,
		Filters: dockerclient.Filters{}.Add("label", "wardyn.run-id="+runID.String())})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("%d containers of %s survived", len(list.Items), runID)
	}
}

// diskImage runs script (which writes /out/d.qcow2) in the QEMU image and imports a containerDisk image
// holding that one file at /disk/d.qcow2. The image is removed when the test ends.
func diskImage(t *testing.T, cli *dockerclient.Client, qemuImage, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out := t.TempDir()
	if err := os.Chmod(out, 0o777); err != nil {
		t.Fatal(err)
	}
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Config:     &container.Config{Image: qemuImage, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), Cmd: []string{"sh", "-ec", script}},
		HostConfig: &container.HostConfig{NetworkMode: "none", Mounts: []mount.Mount{{Type: mount.TypeBind, Source: out, Target: "/out"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	}()
	wait := cli.ContainerWait(ctx, created.ID, dockerclient.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := cli.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-wait.Result:
		if r.StatusCode != 0 {
			t.Fatalf("%q exited %d", script, r.StatusCode)
		}
	case err := <-wait.Error:
		t.Fatal(err)
	}
	disk, err := os.ReadFile(filepath.Join(out, "d.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "disk/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "disk/d.qcow2", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(disk))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(disk); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	ref := "wardyn-winconf-disk:" + uuid.NewString()[:8]
	res, err := cli.ImageImport(ctx, dockerclient.ImageImportSource{Source: &buf, SourceName: "-"}, ref, dockerclient.ImageImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res)
	_ = res.Close()
	t.Cleanup(func() {
		_, _ = cli.ImageRemove(context.Background(), ref, dockerclient.ImageRemoveOptions{Force: true})
	})
	return ref
}

// qmp runs one QMP command on the VM's socket under ctl.
func qmp(t *testing.T, ctl, cmd string, args, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "unix", filepath.Join(ctl, qemu.QMPSocket))
	if err != nil {
		t.Fatal(err)
	}
	q, err := qemu.NewQMP(ctx, conn)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.Execute(ctx, cmd, args, out); err != nil {
		t.Fatalf("QMP %s: %v", cmd, err)
	}
}

// rfbDrawn completes an RFB 3.8 handshake with no authentication on c, then asks for the whole screen
// in raw encoding until a pixel is not black or the limit passes.
func rfbDrawn(c io.ReadWriter, limit time.Duration) (bool, error) {
	if dl, ok := c.(interface{ SetDeadline(time.Time) error }); ok {
		_ = dl.SetDeadline(time.Now().Add(limit + 30*time.Second))
	}
	version := make([]byte, 12)
	if _, err := io.ReadFull(c, version); err != nil || !bytes.HasPrefix(version, []byte("RFB 003.")) {
		return false, fmt.Errorf("RFB version %q: %w", version, err)
	}
	if _, err := c.Write([]byte("RFB 003.008\n")); err != nil {
		return false, err
	}
	var n uint8
	if err := binary.Read(c, binary.BigEndian, &n); err != nil || n == 0 {
		return false, fmt.Errorf("RFB security types: %d, %w", n, err)
	}
	sec := make([]byte, n)
	if _, err := io.ReadFull(c, sec); err != nil || !bytes.Contains(sec, []byte{1}) {
		return false, fmt.Errorf("RFB security types %v: want None: %w", sec, err)
	}
	if _, err := c.Write([]byte{1}); err != nil {
		return false, err
	}
	var result uint32
	if err := binary.Read(c, binary.BigEndian, &result); err != nil || result != 0 {
		return false, fmt.Errorf("RFB security result %d: %w", result, err)
	}
	if _, err := c.Write([]byte{1}); err != nil { // ClientInit, shared
		return false, err
	}
	var init struct {
		W, H    uint16
		Format  [16]byte
		NameLen uint32
	}
	if err := binary.Read(c, binary.BigEndian, &init); err != nil {
		return false, err
	}
	if _, err := io.CopyN(io.Discard, c, int64(init.NameLen)); err != nil {
		return false, err
	}
	bpp := int(init.Format[0]) / 8
	// SetEncodings: raw only.
	if _, err := c.Write([]byte{2, 0, 0, 1, 0, 0, 0, 0}); err != nil {
		return false, err
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		req := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(req[6:], init.W)
		binary.BigEndian.PutUint16(req[8:], init.H)
		if _, err := c.Write(req); err != nil {
			return false, err
		}
		var hdr struct {
			Type, Pad uint8
			Rects     uint16
		}
		if err := binary.Read(c, binary.BigEndian, &hdr); err != nil {
			return false, err
		}
		if hdr.Type != 0 {
			return false, fmt.Errorf("RFB message %d before a framebuffer update", hdr.Type)
		}
		drawn := false
		for range hdr.Rects {
			var r struct {
				X, Y, W, H uint16
				Encoding   int32
			}
			if err := binary.Read(c, binary.BigEndian, &r); err != nil {
				return false, err
			}
			if r.Encoding != 0 {
				return false, fmt.Errorf("RFB encoding %d, asked for raw", r.Encoding)
			}
			px := make([]byte, int(r.W)*int(r.H)*bpp)
			if _, err := io.ReadFull(c, px); err != nil {
				return false, err
			}
			drawn = drawn || slices.ContainsFunc(px, func(b byte) bool { return b != 0 })
		}
		if drawn {
			return true, nil
		}
		time.Sleep(2 * time.Second)
	}
	return false, nil
}
