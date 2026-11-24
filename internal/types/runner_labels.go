// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// RunnerLabelSource says who wrote a runner's label set (the runner_labels table).
type RunnerLabelSource string

const (
	// RunnerLabelsAsserted is what the runner proposed at registration: shown, never trusted.
	RunnerLabelsAsserted RunnerLabelSource = "runner_asserted"
	// RunnerLabelsOperator is what a security operator wrote: the only set records and rules read.
	RunnerLabelsOperator RunnerLabelSource = "operator"
)

// MaxRunnerLabels bounds each source's labels on one runner.
const MaxRunnerLabels = 16

var (
	labelName   = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	labelPrefix = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$`)
)

// ValidRunnerLabelKey is a Kubernetes label key: an optional DNS-subdomain prefix of at most 253 bytes and
// "/", then a name of at most 63 letters, digits, '-', '_' or '.', starting and ending with a letter or digit.
func ValidRunnerLabelKey(k string) bool {
	prefix, name, prefixed := strings.Cut(k, "/")
	if !prefixed {
		return labelName.MatchString(k)
	}
	return len(prefix) <= 253 && labelPrefix.MatchString(prefix) && labelName.MatchString(name)
}

// ValidRunnerLabelValue is a Kubernetes label value: empty, or a name as above. It cannot hold ':', '/' or
// whitespace, so no URL, host:port or path is one.
func ValidRunnerLabelValue(v string) bool { return v == "" || labelName.MatchString(v) }

// ValidateRunnerLabels refuses a set past MaxRunnerLabels and any key or value outside Kubernetes label syntax.
func ValidateRunnerLabels(labels map[string]string) error {
	if len(labels) > MaxRunnerLabels {
		return fmt.Errorf("at most %d labels, not %d", MaxRunnerLabels, len(labels))
	}
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		if !ValidRunnerLabelKey(k) {
			return fmt.Errorf("label key %q is not a Kubernetes label key", k)
		}
		if !ValidRunnerLabelValue(labels[k]) {
			return fmt.Errorf("label %s: value %q is not a Kubernetes label value: at most 63 letters, digits, '-', '_' or '.', starting and ending with a letter or digit", k, labels[k])
		}
	}
	return nil
}

// RunnerLabelsRequest is PUT /runners/{id}/labels: the runner's whole operator label set ({} clears it).
type RunnerLabelsRequest struct {
	Labels map[string]string `json:"labels"`
}
