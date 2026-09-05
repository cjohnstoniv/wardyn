// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
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
			"values redacted) into one tar.gz. Never includes a secret VALUE: audit events carry\n" +
			"only actor/action/target/outcome/time (no Data payload), and compose config lines\n" +
			"matching a password/secret/token/key/DSN pattern are replaced with <redacted>.",
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
// a secret — the one list both regexes below are built from, so a marker can
// never be known to one pass and not the other. Deliberately broad, and
// deliberately not only the names Wardyn itself ships: `docker compose config`
// resolves an OPERATOR's whole environment, so the bundle carries key names
// this repo has never seen. PASSWD/PASSPHRASE/APIKEY are spelled out because
// PASSWORD/_KEY do not contain them.
const secretMarkers = `PASSWORD|PASSWD|PASSPHRASE|SECRET|TOKEN|APIKEY|_KEY|CREDENTIAL|AUTHORIZATION|AUTH|COOKIE|BEARER|_DSN`

// secretLineRe matches a YAML/env-style "KEY: value" or "KEY=value" line
// (optionally list-prefixed with "- ", and optionally commented out with
// "#"/"##") whose key name carries a secretMarkers word — the same shape both
// a raw compose file's `environment:` block and `docker compose config`'s
// resolved output use. The whole rest of the line is the value, which is the
// conservative reading for an unquoted value containing spaces.
// The comment prefix matters on the raw-file fallback path (gatherComposeConfig):
// `docker compose config` itself strips comments, but a support bundle
// gathered from the file on disk still carries them, and "comment out the
// old token" is a routine ops pattern — the value must not survive that.
// Intentionally broad: WARDYN_GROUNDTRUTH_TOKEN_FILE (a file PATH, not a
// secret) also matches and gets redacted, and so does a non-secret key that
// merely contains AUTH — a false-positive redaction is a cost worth paying to
// "refuse to include secret VALUES anywhere".
var secretLineRe = regexp.MustCompile(`(?i)^(\s*(?:#+\s*)?-?\s*)([A-Za-z_][A-Za-z0-9_.]*(?:` + secretMarkers + `)[A-Za-z0-9_.]*)(\s*[:=]\s*)(.*)$`)

// secretPairRe finds a secretMarkers key and its value ANYWHERE in a line, for
// the two shapes that have no bare key at the start of the line at all:
//
//   - a secret NESTED in the value of an innocuously-named variable, which
//     secretLineRe's leading-key check can never see. The shipped compose file's
//     own audit sink is one: WARDYN_AUDIT_SINKS carries a webhook `bearer_token`,
//     and `docker compose config` resolves it to the operator's real SIEM bearer.
//   - a `--flag=value` argv entry under `command:`, whose key starts with dashes
//     (`- --admin-token=...`).
//
// The value stops at the closing quote, or at the first whitespace/JSON
// delimiter for a bare one — never the rest of the line, because these matches
// sit INSIDE a larger structure whose remainder is diagnostic content.
var secretPairRe = regexp.MustCompile(`(?i)("?[A-Za-z0-9_.-]*(?:` + secretMarkers + `)[A-Za-z0-9_.-]*"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}\])]+)`)

// dsnCredsRe is defense-in-depth for a credential embedded in a DSN-shaped
// value on a line secretLineRe's key check didn't catch (e.g. a comment, or
// a value nested inside another field).
var dsnCredsRe = regexp.MustCompile(`://[^:/@\s]+:[^@/\s]+@`)

// redactSecrets replaces every secret-shaped value in b with a fixed marker.
// Non-matching lines pass through byte-for-byte.
func redactSecrets(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if m := secretLineRe.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + m[2] + m[3] + `"<redacted>"`
			continue
		}
		line = secretPairRe.ReplaceAllString(line, `${1}"<redacted>"`)
		lines[i] = dsnCredsRe.ReplaceAllString(line, "://<redacted>@")
	}
	return []byte(strings.Join(lines, "\n"))
}

// writeTarGz writes files (name -> content) to a gzip-compressed tar at path,
// in sorted-name order for a deterministic, diffable bundle.
func writeTarGz(path string, files map[string][]byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	names := slices.Sorted(maps.Keys(files))

	now := time.Now()
	for _, name := range names {
		content := files[name]
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), ModTime: now}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(content); err != nil {
			return err
		}
	}
	return nil
}
