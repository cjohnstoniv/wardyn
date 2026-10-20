// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package guestagent is the contract between wardyn-runnerd and the Wardyn guest agent inside a Linux
// VM sandbox. The agent serves HTTP/2 with prior knowledge over one virtio-serial port
// (PortName); runnerd reaches it through the QEMU chardev socket in the run's ctl directory. One byte
// stream carries every session (the harness, terminals, exec streams) with HTTP/2's own flow
// control per session; there is no network listener in the guest.
//
// Requests:
//
//	GET  /v1/ready                      Ready
//	POST /v1/init                       Init, once per boot
//	POST /v1/exec                       full-duplex body of frames: ExecRequest first
//	GET  /v1/exec/{id}                  ExecState
//	GET  /v1/exec/{id}/output?offset=N  a detached exec's output from N, following until it exits
//	GET  /v1/agent                      ExecState of the latest detached exec
package guestagent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
)

// PortName is the virtio-serial port the agent serves; in the guest it is /dev/virtio-ports/PortName.
const PortName = "org.wardyn.agent.0"

// AgentUID and AgentGID are the image contract's agent user (as in the container images): every exec
// runs as it, with its home at AgentHome.
const (
	AgentUID  = 1000
	AgentGID  = 1000
	AgentHome = "/home/agent"
)

// Ready is GET /v1/ready's answer.
type Ready struct {
	Version string `json:"version"`
}

// Init is POST /v1/init: what the sandbox holds before its first exec. Env and SecretEnv reach every
// exec (SecretEnv is held in the agent's memory only, never written to the guest's disk); Files are
// written as root before the answer; Mounts are virtiofs shares mounted at their targets.
type Init struct {
	Env       map[string]string `json:"env,omitempty"`
	SecretEnv map[string]string `json:"secret_env,omitempty"`
	Files     []File            `json:"files,omitempty"`
	Mounts    []Mount           `json:"mounts,omitempty"`
}

// File is one managed file. AgentOwned files belong to the agent user; the rest to root.
type File struct {
	Path       string      `json:"path"`
	Mode       fs.FileMode `json:"mode"`
	AgentOwned bool        `json:"agent_owned,omitempty"`
	Content    []byte      `json:"content"`
}

// Mount is one virtiofs share: the QEMU device tag and where it is mounted.
type Mount struct {
	Tag      string `json:"tag"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ExecRequest is the first frame of POST /v1/exec. Detach starts a process that outlives the
// connection (the harness): the answer is one FrameStarted, and its output goes to the guest's
// output file. Otherwise the session is attached: stdin and resizes go up, stdout, stderr and the
// exit code come down. TTY gives the process a pseudo-terminal (stderr then arrives as stdout).
type ExecRequest struct {
	Argv   []string `json:"argv"`
	Env    []string `json:"env,omitempty"`
	TTY    bool     `json:"tty,omitempty"`
	Cols   uint16   `json:"cols,omitempty"`
	Rows   uint16   `json:"rows,omitempty"`
	Detach bool     `json:"detach,omitempty"`
}

// ExecState is a detached exec's state. ExitCode is set once it has exited.
type ExecState struct {
	ID       string `json:"id"`
	Running  bool   `json:"running"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// Frame types. A frame is one type byte, a 4-byte big-endian length and the payload.
const (
	FrameSpec       byte = 0x00 // up: ExecRequest JSON, always first
	FrameStdin      byte = 0x01 // up
	FrameStdinClose byte = 0x02 // up: no payload
	FrameResize     byte = 0x03 // up: cols, rows as two big-endian uint16
	FrameStdout     byte = 0x11 // down
	FrameStderr     byte = 0x12 // down
	FrameExit       byte = 0x13 // down: exit code as a big-endian int32; the last frame
	FrameError      byte = 0x14 // down: a message; the last frame
	FrameStarted    byte = 0x15 // down: a detached exec's id; the only frame
)

// MaxFrame bounds a frame's payload, so neither side allocates what the other names.
const MaxFrame = 64 << 10

// ErrFrameTooLarge is a frame whose length is over MaxFrame.
var ErrFrameTooLarge = errors.New("guestagent: frame over the size limit")

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame; io.EOF only at a frame boundary.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("guestagent: truncated frame header: %w", err)
		}
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("guestagent: truncated frame: %w", err)
	}
	return hdr[0], payload, nil
}

// int32Payload and uint16Pair are the fixed-size payloads.
func int32Payload(v int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(v)))
	return b[:]
}

func readInt32(p []byte) (int, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("guestagent: exit frame of %d bytes", len(p))
	}
	return int(int32(binary.BigEndian.Uint32(p))), nil
}

func uint16Pair(a, b uint16) []byte {
	var p [4]byte
	binary.BigEndian.PutUint16(p[:2], a)
	binary.BigEndian.PutUint16(p[2:], b)
	return p[:]
}

func readUint16Pair(p []byte) (uint16, uint16, error) {
	if len(p) != 4 {
		return 0, 0, fmt.Errorf("guestagent: resize frame of %d bytes", len(p))
	}
	return binary.BigEndian.Uint16(p[:2]), binary.BigEndian.Uint16(p[2:]), nil
}
