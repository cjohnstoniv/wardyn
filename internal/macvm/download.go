// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DownloadRange is the size of one resumable range of the restore image. A
// resume re-fetches at most one range.
const DownloadRange = 64 << 20

// maxRangeFailures is how many failed ranges in a row end the download.
const maxRangeFailures = 3

// DownloadFailurePrefix starts the error a restore image download ends with; the cause follows it.
// The download wraps its own cause in it twice over (fetch, then the range), and a caller strips it
// until none is left.
const DownloadFailurePrefix = "macvm: restore image download: "

// pinnedHostAdvice is the cause a certificate-pinning refusal ends the download with: the host twice, then
// the image URL prepare asked for. Apple pins its update CDN, so the platform verifier refuses any chain a
// TLS-inspecting proxy re-signs; the refusal alone does not prove a proxy, so the advice says likely.
const pinnedHostAdvice = "the certificate chain for %s failed Apple's pinning check; a proxy that inspects TLS is the " +
	"likely cause: exempt %s from TLS inspection and run prepare again (the image is %s), or download that image on " +
	"another network and run prepare with --ipsw <file> --sha256 <hex>, taking the SHA-256 from a source you trust"

// isPinningRefusal says err is the platform verifier refusing a pinned host's chain. macOS reports it
// as a bare "x509: ... certificates do not meet pinning requirements" with no typed error to match, so
// the text is the only signal.
func isPinningRefusal(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "x509:") && strings.Contains(msg, "pinning requirements")
}

type downloader struct {
	client *http.Client
	power  PowerSource
	poll   time.Duration
	rng    int64
	// paused is told when the download pauses for battery (true) and resumes.
	paused   func(bool)
	progress func(done, total int64)
	// retrying is told that the range at off failed and is fetched again; total is -1 until known.
	retrying func(off, total int64)
}

// fetch downloads url into part, resuming from part's current size, one range
// at a time. It checks power before each range: on battery it pauses at the
// end of the range in flight and resumes from the next one on AC. A failed
// range is truncated back to its start and fetched again.
func (d *downloader) fetch(ctx context.Context, url, part string) error {
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	off, total := st.Size(), int64(-1)
	failures := 0
	for total < 0 || off < total {
		if err := d.waitAC(ctx); err != nil {
			return err
		}
		end := off + d.rng - 1
		if total >= 0 && end >= total {
			end = total - 1
		}
		n, t, err := d.fetchRange(ctx, url, f, off, end)
		if t >= 0 {
			total = t
		}
		if err != nil {
			if terr := f.Truncate(off); terr != nil {
				return terr
			}
			if _, serr := f.Seek(off, io.SeekStart); serr != nil {
				return serr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isPinningRefusal(err) {
				// A proxy that inspects TLS refuses every range the same way; retrying only waits.
				host := url
				if u, perr := neturl.Parse(url); perr == nil && u.Hostname() != "" {
					host = u.Hostname()
				}
				return fmt.Errorf(DownloadFailurePrefix+pinnedHostAdvice+": %w", host, host, url, err)
			}
			if failures++; failures >= maxRangeFailures {
				return fmt.Errorf(DownloadFailurePrefix+"%w", err)
			}
			if d.retrying != nil {
				d.retrying(off, total)
			}
			continue
		}
		failures = 0
		off += n
		if d.progress != nil {
			d.progress(off, total)
		}
	}
	return f.Sync()
}

// waitAC returns once the Mac is on AC power.
func (d *downloader) waitAC(ctx context.Context) error {
	pausedNow := false
	defer func() {
		if pausedNow && d.paused != nil {
			d.paused(false)
		}
	}()
	for {
		ac, err := d.power.OnAC(ctx)
		if err == nil && ac {
			return nil
		}
		if !pausedNow {
			pausedNow = true
			if d.paused != nil {
				d.paused(true)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.poll):
		}
	}
}

// fetchRange writes bytes [off, end] at f's current offset and returns how
// many it wrote and the image's total size (-1 when unknown).
func (d *downloader) fetchRange(ctx context.Context, url string, f *os.File, off, end int64) (int64, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, -1, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, -1, err
	}
	defer resp.Body.Close()
	total := int64(-1)
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, t, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return 0, -1, err
		}
		if start != off {
			return 0, t, fmt.Errorf("macvm: server sent range from %d, asked %d", start, off)
		}
		total = t
	case http.StatusRequestedRangeNotSatisfiable:
		return 0, off, nil // already complete
	default:
		return 0, -1, fmt.Errorf(DownloadFailurePrefix+"HTTP %d", resp.StatusCode)
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, end-off+1))
	if err == nil && n != end-off+1 && (total < 0 || off+n < total) {
		err = io.ErrUnexpectedEOF
	}
	return n, total, err
}

// parseContentRange reads "bytes <start>-<end>/<total>".
func parseContentRange(v string) (start, total int64, err error) {
	rest, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, -1, fmt.Errorf("macvm: bad Content-Range %q", v)
	}
	rng, tot, ok := strings.Cut(rest, "/")
	s, _, ok2 := strings.Cut(rng, "-")
	if !ok || !ok2 {
		return 0, -1, fmt.Errorf("macvm: bad Content-Range %q", v)
	}
	if start, err = strconv.ParseInt(s, 10, 64); err != nil {
		return 0, -1, fmt.Errorf("macvm: bad Content-Range %q", v)
	}
	if total, err = strconv.ParseInt(tot, 10, 64); err != nil {
		return 0, -1, fmt.Errorf("macvm: Content-Range %q has no total", v)
	}
	return start, total, nil
}

// fileSHA256 is the hex SHA-256 of a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
