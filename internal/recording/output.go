// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidCast identifies a recording that cannot establish an output stream.
// Its error text never contains sandbox-controlled recording content.
var ErrInvalidCast = errors.New("recording: invalid asciicast v2")

// CopyOutput decodes a joined asciicast v2 stream into consecutive output
// payloads. Mask dst as one stream: event and part boundaries may split secrets.
func CopyOutput(ctx context.Context, dst io.Writer, src io.Reader) error {
	lines := bufio.NewScanner(src)
	// An uploaded part may contain one event up to the existing part byte cap.
	// Apply the same ceiling to FS recordings, which may bypass HTTP admission.
	lines.Buffer(make([]byte, 32<<10), maxCastBytes+1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !lines.Scan() {
		if err := lines.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
			return err
		}
		return ErrInvalidCast
	}
	var header CastHeader
	if err := json.Unmarshal(lines.Bytes(), &header); err != nil || header.Version != 2 || header.Width <= 0 || header.Height <= 0 {
		return fmt.Errorf("%w: header", ErrInvalidCast)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !lines.Scan() {
			if errors.Is(lines.Err(), bufio.ErrTooLong) {
				return ErrInvalidCast
			}
			return lines.Err()
		}
		if len(lines.Bytes()) == 0 {
			continue
		}
		data, output, err := outputEvent(lines.Bytes())
		if err != nil {
			return err
		}
		if output {
			n, err := io.WriteString(dst, data)
			if err != nil {
				return err
			}
			if n != len(data) {
				return io.ErrShortWrite
			}
		}
	}
}

func outputEvent(line []byte) (string, bool, error) {
	var fields []json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil || len(fields) != 3 {
		return "", false, fmt.Errorf("%w: event", ErrInvalidCast)
	}
	var elapsed float64
	var kind, data string
	if err := json.Unmarshal(fields[0], &elapsed); err != nil || elapsed < 0 || string(fields[0]) == "null" {
		return "", false, fmt.Errorf("%w: event time", ErrInvalidCast)
	}
	if err := json.Unmarshal(fields[1], &kind); err != nil || kind == "" {
		return "", false, fmt.Errorf("%w: event kind", ErrInvalidCast)
	}
	if err := json.Unmarshal(fields[2], &data); err != nil || string(fields[2]) == "null" {
		return "", false, fmt.Errorf("%w: event data", ErrInvalidCast)
	}
	return data, kind == "o", nil
}
