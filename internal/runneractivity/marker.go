// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runneractivity carries only current-connection activity between the
// proxy and daemon. It is not audit storage; reconnect invalidates all markers.
package runneractivity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/spoolowner"
)

const epochFile = "live-epoch.json"
const signalFile = "live-activity.json"
const maxMarker = 256

// Marker binds one newly admitted request to the connection present at admission.
type Marker struct {
	Epoch uuid.UUID `json:"epoch"`
	ID    uuid.UUID `json:"id"`
}

// Publish switches the local channel to a fresh started connection. Nil removes
// admission authority. Reads never accept a partly written epoch.
func Publish(dir string, epoch uuid.UUID) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if epoch == uuid.Nil {
		err := root.Remove(epochFile)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return write(root, epochFile, Marker{Epoch: epoch, ID: epoch})
}

// Capture must run at request admission, not when an old request later finishes.
func Capture(dir string) uuid.UUID {
	m, err := read(dir, epochFile)
	if err != nil || m.ID != m.Epoch {
		return uuid.Nil
	}
	return m.Epoch
}

// Record coalesces live activity. Caller serializes writes; no durable replay
// path calls it. Offline admission (nil epoch) cannot produce an activity signal.
func Record(dir string, epoch uuid.UUID) error {
	if epoch == uuid.Nil {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return write(root, signalFile, Marker{Epoch: epoch, ID: uuid.New()})
}

// Read observes a whole replacement without deleting it: deleting after a read
// could erase a newer concurrent writer. The daemon deduplicates ID in memory.
func Read(dir string) (Marker, error) { return read(dir, signalFile) }

func read(dir, name string) (Marker, error) {
	var out Marker
	root, err := os.OpenRoot(dir)
	if err != nil {
		return out, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return out, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	link, err := root.Lstat(name)
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxMarker || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, link) {
		return out, errors.New("unsafe live activity marker")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMarker+1))
	if err != nil || len(data) > maxMarker {
		return out, errors.New("oversized live activity marker")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) || out.Epoch == uuid.Nil || out.ID == uuid.Nil {
		return Marker{}, errors.New("invalid live activity marker")
	}
	return out, nil
}

func write(root *os.Root, name string, m Marker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	temp := name + "." + uuid.NewString()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp) //nolint:errcheck // rename removes the temporary name
	// The directory is the run's spool, shared by wardyn-runnerd and the proxy: the file keeps its owner.
	oerr := spoolowner.Keep(root, f)
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(oerr, werr, cerr); err != nil {
		return err
	}
	return root.Rename(temp, name)
}
