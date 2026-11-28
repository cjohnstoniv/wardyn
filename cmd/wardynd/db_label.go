// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// Every connection wardynd opens names itself, so the activation of issuer portals can tell a 0.10.2 process
// from an older one or from anything else on the database (internal/store/store_portal_activation.go reads
// the label). Two shapes, and only these: the server's wardynd/<version>/<replica>, and a subcommand's
// wardynd-<tool>/<version>. The version is the bare release, never the build suffix.

// dbLabelMax is what Postgres keeps of an application_name (NAMEDATALEN - 1).
const dbLabelMax = 63

var (
	replicaIDOnce sync.Once
	replicaIDVal  string
	replicaBad    = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// replicaID names this process in its database label and in the console-issuer record: the host (the pod
// name under Kubernetes) cut to what the label has room for, and a per-process suffix. Its characters are
// letters, digits, dot, underscore and hyphen, and it starts with a letter or digit.
func replicaID() string {
	replicaIDOnce.Do(func() {
		host, _ := os.Hostname()
		replicaIDVal = buildReplicaID(host, uuid.NewString()[:8], version.Version)
	})
	return replicaIDVal
}

// buildReplicaID is replicaID for a host, a suffix and a version: the label wardynd/<version>/<id> is at most
// dbLabelMax characters, so a long host is cut and the suffix kept.
func buildReplicaID(host, suffix, ver string) string {
	host = strings.TrimLeft(replicaBad.ReplaceAllString(host, "-"), "-._")
	room := dbLabelMax - len("wardynd//") - len(ver) - len(suffix) - 1
	if room < 0 {
		room = 0
	}
	if len(host) > room {
		host = host[:room]
	}
	host = strings.TrimRight(host, "-._")
	if host == "" {
		return suffix
	}
	return host + "-" + suffix
}

// serverDBLabel is the label of the server process: wardynd/<version>/<replica>.
func serverDBLabel() string { return "wardynd/" + version.Version + "/" + replicaID() }

// toolDBLabel is the label of subcommand tool: wardynd-<tool>/<version>.
func toolDBLabel(tool string) string { return "wardynd-" + tool + "/" + version.Version }

// serverDBOption and toolDBOption are the connect options carrying those labels.
func serverDBOption() db.ConnectOption { return db.WithApplicationName(serverDBLabel()) }

func toolDBOption(tool string) db.ConnectOption { return db.WithApplicationName(toolDBLabel(tool)) }
