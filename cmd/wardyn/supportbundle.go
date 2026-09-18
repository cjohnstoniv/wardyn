// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// supportBundleCmd is D11's fleet-diagnosis lane: it gathers what a support
// ticket needs — daemon version/health, first-run setup readiness, a bounded
// content-free tail of the recent audit feed, and the compose deployment's
// config with every secret-shaped value redacted — into one tar.gz an
// operator can attach. Every gather step is best-effort: a piece the caller
// can't reach (no admin token, no compose file, an unreachable daemon) lands
// as a short ".error.txt"/".note.txt" entry instead of aborting the whole
// bundle, since a partial bundle is still useful for a ticket and this is
// often run BECAUSE something is already wrong.
func supportBundleCmd(client clientFn) *cobra.Command {
	var outPath, composeFile string
	var auditLimit int
	cmd := &cobra.Command{
		Use:   "support-bundle",
		Short: "Gather version/healthz/setup-status/audit-tail/compose-config into a tar.gz for a support ticket",
		Long: "Gather daemon version, /healthz, first-run setup status, a bounded content-free\n" +
			"tail of the recent audit feed, and the compose deployment's config (secret-shaped\n" +
			"values redacted) into one tar.gz. Audit events carry only\n" +
			"actor/action/target/outcome/time (no Data payload). Secret-shaped YAML values\n" +
			"and opaque values containing credential-shaped fields are redacted. Comments\n" +
			"and unparseable Compose YAML are omitted. Review the bundle before sharing it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c := client()
			files := map[string][]byte{}

			files["cli-version.txt"] = []byte(version.Version + "\n")

			if raw, err := c.Healthz(ctx); err != nil {
				files["healthz.error.txt"] = []byte(err.Error() + "\n")
			} else {
				files["healthz.json"] = prettyJSON(raw)
			}

			if raw, err := c.SetupStatus(ctx); err != nil {
				files["setup-status.error.txt"] = []byte(err.Error() + "\n")
			} else {
				files["setup-status.json"] = prettyJSON(raw)
			}

			if events, err := c.RecentAuditEvents(ctx, sdk.ListOpts{Limit: auditLimit}); err != nil {
				files["audit-tail.error.txt"] = []byte(err.Error() + "\n")
			} else {
				files["audit-tail.json"] = contentFreeAuditJSON(events)
			}

			if cfg, note := gatherComposeConfig(composeFile); cfg != nil {
				files["compose-config.redacted.yaml"] = redactSecrets(cfg)
			} else {
				files["compose-config.note.txt"] = []byte(note + "\n")
			}

			if outPath == "" {
				outPath = fmt.Sprintf("wardyn-support-bundle-%s.tar.gz", time.Now().UTC().Format("20060102T150405Z"))
			}
			if err := writeTarGz(outPath, files); err != nil {
				return fmt.Errorf("write support bundle: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", outPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "output tar.gz path (default wardyn-support-bundle-<timestamp>.tar.gz)")
	cmd.Flags().StringVar(&composeFile, "compose-file", "deploy/compose/docker-compose.yaml",
		"compose file to include, redacted (skipped with a note if not found and `docker compose config` also fails)")
	cmd.Flags().IntVar(&auditLimit, "audit-limit", 200,
		"max recent audit events to include (content-free: actor/action/target/outcome/time only, never the Data payload)")
	return cmd
}

// bundleAuditEvent is the content-free projection of types.AuditEvent the
// support bundle writes: every envelope field a support engineer needs to
// spot a pattern (WHO did WHAT to WHAT and whether it worked), never the
// event's Data payload — which can carry command argv, hostnames, or other
// operational detail well beyond what a bounded bundle should carry off a
// laptop by default.
type bundleAuditEvent struct {
	Time      time.Time       `json:"time"`
	ActorType types.ActorType `json:"actor_type"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Target    string          `json:"target,omitempty"`
	Outcome   string          `json:"outcome"`
}

func contentFreeAuditJSON(events []types.AuditEvent) []byte {
	out := make([]bundleAuditEvent, len(events))
	for i, e := range events {
		out[i] = bundleAuditEvent{
			Time: e.Time, ActorType: e.ActorType, Actor: e.Actor,
			Action: e.Action, Target: e.Target, Outcome: e.Outcome,
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return []byte("[]")
	}
	return b
}

// prettyJSON indents raw JSON for readability in the bundle; on a malformed
// input (should not happen — it came from json.Decode server-side) it falls
// back to the raw bytes rather than dropping the file.
func prettyJSON(raw []byte) []byte {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return raw
	}
	return buf.Bytes()
}

// gatherComposeConfig returns the compose config to include, or nil plus an
// explanatory note. It prefers the LIVE resolved config (`docker compose -f
// path config`, which expands ${VAR} against the actual environment — the
// values an operator's deployment is really running with) and falls back to
// the raw file on disk when the docker CLI isn't available or the file isn't
// a live compose project (e.g. this CLI running outside the repo/deploy
// dir). Neither path is redacted here — the caller always runs redactSecrets
// on whatever comes back.
func gatherComposeConfig(path string) ([]byte, string) {
	if strings.TrimSpace(path) == "" {
		return nil, "no compose file configured (--compose-file)"
	}
	if out, err := exec.Command("docker", "compose", "-f", path, "config").Output(); err == nil {
		return out, ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("compose config not gathered: %v (looked for %s; pass --compose-file, or run `docker compose -f <file> config` yourself and attach it)", err, path)
	}
	return raw, ""
}

// secretMarkers is THE vocabulary of key-name substrings that mark a value as
// a secret — shared by structured-key and embedded-value matching, so a marker can
// never be known to one pass and not the other. Deliberately broad, and
// deliberately not only the names Wardyn itself ships: `docker compose config`
// resolves an OPERATOR's whole environment, so the bundle carries key names
// this repo has never seen. PASSWD/PASSPHRASE/APIKEY are spelled out because
// PASSWORD/_KEY do not contain them.
const secretMarkers = `PASSWORD|PASSWD|PASSPHRASE|SECRET|TOKEN|APIKEY|_KEY|CREDENTIAL|AUTHORIZATION|AUTH|COOKIE|BEARER|_DSN`

// Opaque YAML scalars can contain nested credentials (WARDYN_AUDIT_SINKS' JSON)
// or command flags. A match redacts the whole scalar, not a guessed substring.
var secretPairRe = regexp.MustCompile(`(?i)("?[A-Za-z0-9_.-]*(?:` + secretMarkers + `)[A-Za-z0-9_.-]*"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}\])]+)`)

// DSN credentials are sensitive even when their containing key is innocuous.
var dsnCredsRe = regexp.MustCompile(`://[^:/@\s]+:[^@/\s]+@`)

// writeTarGzWriter tar+gzip-encodes files (name -> content) into w, in
// sorted-name order for a deterministic, diffable bundle. It closes tw then
// gz itself — in the order data actually flows out, tar trailer before gzip
// footer — and returns the FIRST error from either the per-entry writes or
// those two closes: on ext4, ENOSPC usually surfaces only at the final flush
// (the two zero trailer blocks tar.Writer.Close writes, or the footer
// gzip.Writer.Close writes), by which point every earlier Write already
// "succeeded" into the OS's page cache. w itself is left open — the caller
// owns it.
func writeTarGzWriter(w io.Writer, files map[string][]byte) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	var firstErr error
	setErr := func(err error) {
		if firstErr == nil && err != nil {
			firstErr = err
		}
	}

	now := time.Now()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content := files[name]
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), ModTime: now}
		if err := tw.WriteHeader(hdr); err != nil {
			setErr(err)
			break
		}
		if _, err := tw.Write(content); err != nil {
			setErr(err)
			break
		}
	}
	setErr(tw.Close())
	setErr(gz.Close())
	return firstErr
}

// writeTarGz writes files (name -> content) to a gzip-compressed tar at path.
// Written to a .part file and renamed on success (finalizePartFile, shared
// with `run recording`'s download) so a flush failure midway through never
// leaves a truncated bundle sitting at path.
func writeTarGz(path string, files map[string][]byte) error {
	partPath := path + ".part"
	f, err := os.Create(partPath)
	if err != nil {
		return err
	}
	writeErr := writeTarGzWriter(f, files)
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	return finalizePartFile(partPath, path, writeErr)
}
