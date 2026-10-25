// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// The test binary doubles as the command: re-executed with this variable set it
// runs main, so the flags and the real stdin/stdout framing are what is tested.
func TestMain(m *testing.M) {
	if os.Getenv("WARDYN_VMNET_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func vmnetCmd(args ...string) *exec.Cmd {
	c := exec.Command(os.Args[0], args...)
	c.Env = append(os.Environ(), "WARDYN_VMNET_AS_MAIN=1")
	return c
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// discoverFrame is a guest's DHCP DISCOVER as one Ethernet frame.
func discoverFrame() []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = 1, 1, 6
	copy(b[28:], "\x52\x54\x00\x12\x34\x56")
	copy(b[236:], []byte{99, 130, 83, 99})
	b = append(b, 53, 1, 1, 255)
	total := header.EthernetMinimumSize + header.IPv4MinimumSize + header.UDPMinimumSize + len(b)
	fr := make([]byte, total)
	header.Ethernet(fr).Encode(&header.EthernetFields{SrcAddr: "\x52\x54\x00\x12\x34\x56",
		DstAddr: "\xff\xff\xff\xff\xff\xff", Type: header.IPv4ProtocolNumber})
	ip := header.IPv4(fr[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(total - header.EthernetMinimumSize), TTL: 64,
		Protocol: uint8(header.UDPProtocolNumber), SrcAddr: tcpip.AddrFrom4([4]byte{}), DstAddr: tcpip.AddrFrom4([4]byte{255, 255, 255, 255})})
	ip.SetChecksum(^ip.CalculateChecksum())
	u := header.UDP(ip.Payload())
	u.Encode(&header.UDPFields{SrcPort: 68, DstPort: 67, Length: uint16(header.UDPMinimumSize + len(b))})
	copy(u.Payload(), b)
	return fr
}

func TestFlagsRequireExactlyOneAttachMode(t *testing.T) {
	for _, args := range [][]string{{}, {"-listen", "/tmp/x.sock", "-stdio"}, {"-prep"}} {
		if code := exitCode(vmnetCmd(args...).Run()); code != 2 {
			t.Errorf("args %v: exit %d, want 2", args, code)
		}
	}
}

func TestStdioCarriesFramesOverStdinAndStdout(t *testing.T) {
	for _, args := range [][]string{{"-stdio"}, {"-stdio", "-prep"}} {
		cmd := vmnetCmd(args...)
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		fr := discoverFrame()
		hdr := binary.BigEndian.AppendUint32(nil, uint32(len(fr)))
		if _, err := in.Write(append(hdr, fr...)); err != nil {
			t.Fatal(err)
		}
		// stdout is frames only: a 4-byte length, then the DHCP offer.
		var lb [4]byte
		if _, err := io.ReadFull(out, lb[:]); err != nil {
			t.Fatalf("%v: no frame on stdout: %v (stderr %s)", args, err, stderr.String())
		}
		reply := make([]byte, binary.BigEndian.Uint32(lb[:]))
		if _, err := io.ReadFull(out, reply); err != nil {
			t.Fatal(err)
		}
		bootp := reply[header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize:]
		if bootp[0] != 2 || !bytes.Equal(bootp[16:20], []byte{10, 0, 2, 15}) {
			t.Fatalf("%v: reply is not an offer for 10.0.2.15: % x", args, bootp[:20])
		}
		// The guest side closing stdin ends the relay cleanly.
		_ = in.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if code := exitCode(err); code != 0 {
				t.Errorf("%v: exit %d after EOF (stderr %s)", args, code, stderr.String())
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Errorf("%v: still running after EOF on stdin", args)
		}
	}
}

func TestStdioEndsOnAnOutOfBoundsFrameLength(t *testing.T) {
	cmd := vmnetCmd("-stdio", "-prep")
	in, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = in.Write(binary.BigEndian.AppendUint32(nil, 1<<30))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if exitCode(err) != 1 {
			t.Fatalf("exit %d, want 1", exitCode(err))
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("relay kept running after a 1 GiB frame length")
	}
}
