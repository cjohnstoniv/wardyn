// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// gtSecretPatch merges data into the one Secret named by
// WARDYN_GROUNDTRUTH_TOKEN_SECRET. The Kubernetes build supplies it
// (gt_secret_k8s.go); a build without Kubernetes support refuses the setting.
type gtSecretPatch func(ctx context.Context, data map[string][]byte) error

// Keys of the sensor Secret. The sensor mounts the Secret as a directory, so
// these are the file names WARDYN_GROUNDTRUTH_TOKEN_FILE and
// WARDYN_CONTROL_PLANE_CA_FILE point at; the CA key is the name the file mode
// publishes beside the token (hopCAFileName).
const gtSecretTokenKey = "token"

// secretTokenWriter patches the token, and the internal CA when there is one,
// into the sensor Secret. With no internal TLS listener there is no CA, and
// the patch carries the token alone rather than blanking the key.
func secretTokenWriter(patch gtSecretPatch, caPEM string) gtTokenWriter {
	return func(ctx context.Context, token string) error {
		data := map[string][]byte{gtSecretTokenKey: []byte(token)}
		if caPEM != "" {
			data[hopCAFileName] = []byte(caPEM)
		}
		if err := patch(ctx, data); err != nil {
			return fmt.Errorf("patch the sensor Secret: %w", err)
		}
		return nil
	}
}

var (
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// parseGroundtruthTokenSecret reads WARDYN_GROUNDTRUTH_TOKEN_SECRET's
// <namespace>/<name>, the same names Kubernetes accepts.
func parseGroundtruthTokenSecret(v string) (ns, name string, err error) {
	ns, name, ok := strings.Cut(v, "/")
	if !ok || len(ns) > 63 || !dns1123Label.MatchString(ns) || len(name) > 253 || !dns1123Subdomain.MatchString(name) {
		return "", "", fmt.Errorf("WARDYN_GROUNDTRUTH_TOKEN_SECRET=%q: want <namespace>/<secret-name>", v)
	}
	return ns, name, nil
}

// groundtruthTokenDelivery reads where the ground-truth sensor's token goes:
// the shared file (WARDYN_GROUNDTRUTH_TOKEN_FILE, Compose), the named Secret
// (WARDYN_GROUNDTRUTH_TOKEN_SECRET, the Kubernetes sensor), or nowhere (both
// unset: no rotator). k8sSensor reports the Secret, which is what tells
// dispatch the deployment runs the Kubernetes sensor. Both set, a malformed
// name, or a Secret on a build without Kubernetes support refuses boot.
// groundtruthTokenTarget names where the rotator writes, for the boot log: "file:<path>", "secret:<ns>/<name>"
// or "" when neither is set.
func groundtruthTokenTarget() string {
	if ref := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_SECRET")); ref != "" {
		return "secret:" + ref
	}
	if file := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE")); file != "" {
		return "file:" + file
	}
	return ""
}

func groundtruthTokenDelivery(hop *hopTLS) (write gtTokenWriter, k8sSensor bool, err error) {
	file := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE"))
	ref := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_SECRET"))
	switch {
	case file != "" && ref != "":
		return nil, false, errors.New("set one of WARDYN_GROUNDTRUTH_TOKEN_FILE (Compose) or WARDYN_GROUNDTRUTH_TOKEN_SECRET (Kubernetes), not both")
	case ref != "":
		ns, name, err := parseGroundtruthTokenSecret(ref)
		if err != nil {
			return nil, false, err
		}
		patch, err := newGroundtruthSecretPatch(ns, name)
		if err != nil {
			return nil, false, err
		}
		return secretTokenWriter(patch, hop.caCertPEM()), true, nil
	case file != "":
		return fileTokenWriter(file), false, nil
	}
	return nil, false, nil
}
