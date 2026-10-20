// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// exitOnBadPortRule refuses to boot on a WARDYN_EGRESS_PORT_RULE the proxy
// would refuse, rather than start every container run's sidecar on a value
// it cannot load.
func exitOnBadPortRule(f *bootFlags) {
	if v := *f.egressPortRule; v == "" || !proxy.ValidPortRule(v) {
		fmt.Fprintf(flag.CommandLine.Output(), "invalid WARDYN_EGRESS_PORT_RULE=%q: want off, dry_run or on\n", v)
		os.Exit(2)
	}
}

// egressPortRule is the run proxy's port_rule for a validated knob: off is
// the key left out, so a proxy image older than the rule still loads the
// config.
func egressPortRule(v string) string {
	if v == proxy.PortRuleOff {
		return ""
	}
	return v
}
