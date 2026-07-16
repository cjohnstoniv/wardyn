// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-runner is Wardyn's data-plane runner. It implements the
// internal/runner.Runner contract via the docker driver (k8s driver lands
// later) and is consumed as a library by the control plane.
//
// For v0 it ALSO ships a standalone mode for manual and conformance testing:
// given a JSON SandboxSpec file, it creates one governed sandbox, optionally
// execs a command in it, prints status, and (unless -keep) tears it down. It
// does NOT poll the control plane for PENDING runs — scheduling is the control
// plane's job.
//
// Usage:
//
//	wardyn-runner -spec spec.json [-exec '<argv>'] [-keep] \
//	  [-control-plane http://wardynd:8080] [-token <run-token>] \
//	  [-proxy-image wardyn-proxy:dev] [-proxy-binary /path/to/wardyn-proxy]
//
// Built only with `-tags docker`: the docker driver is a build-tagged add-on
// (parity rule — the default build carries zero target-specific code). The
// default (!docker) build provides a stub main in main_nodocker.go.

//go:build docker

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	dockerdriver "github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fileSpec is the JSON shape accepted by -spec. It mirrors runner.SandboxSpec
// but uses JSON-friendly field names and lets run_id be omitted (a fresh UUID
// is generated). Secrets MUST NOT appear here (invariant 1).
type fileSpec struct {
	RunID            string                 `json:"run_id,omitempty"`
	Image            string                 `json:"image"`
	ConfinementClass types.ConfinementClass `json:"confinement_class,omitempty"`
	Env              map[string]string      `json:"env,omitempty"`
	Resources        struct {
		CPUMillis int64 `json:"cpu_millis,omitempty"`
		MemoryMiB int64 `json:"memory_mib,omitempty"`
		DiskMiB   int64 `json:"disk_mib,omitempty"`
	} `json:"resources,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-runner:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		specPath     = flag.String("spec", "", "path to a JSON SandboxSpec (standalone mode)")
		execArg      = flag.String("exec", "", "space-separated argv to exec in the sandbox after create")
		keep         = flag.Bool("keep", false, "do not tear down the sandbox on exit")
		controlPlane = flag.String("control-plane", "", "control plane base URL (passed to the proxy sidecar)")
		token        = flag.String("token", "", "run token authenticating sidecars to the control plane")
		proxyImage   = flag.String("proxy-image", "", "OCI image for the wardyn-proxy sidecar")
		proxyBinary  = flag.String("proxy-binary", "", "host path to a wardyn-proxy binary to bind-mount into the sidecar (v0 dev)")
		internalNet  = flag.String("internal-network", "wardyn-internal", "control-plane-facing bridge network the proxy joins")
		record       = flag.Bool("record", false, "enable wardyn-rec PTY session recording on -exec")
		capsOnly     = flag.Bool("capabilities", false, "print driver capabilities as JSON and exit")
	)
	flag.Parse()

	drv, err := dockerdriver.New(dockerdriver.Config{
		ProxyImage:          *proxyImage,
		ProxyBinaryHostPath: *proxyBinary,
		InternalNetwork:     *internalNet,
		Record:              *record,
	})
	if err != nil {
		return err
	}
	// Present the runner.Runner surface via the orchestrator (the OCI substrate
	// behind the same multiplexer the control plane uses).
	run := orchestrator.New(drv)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *capsOnly {
		caps, err := run.Capabilities(ctx)
		if err != nil {
			return err
		}
		return printJSON(caps)
	}

	if *specPath == "" {
		return fmt.Errorf("standalone mode requires -spec (or use -capabilities)")
	}

	spec, err := loadSpec(*specPath)
	if err != nil {
		return err
	}
	spec.ProxyConfig = runner.ProxyConfig{RunToken: *token, ControlPlaneURL: *controlPlane}

	sb, err := run.CreateSandbox(ctx, spec)
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	fmt.Printf("created sandbox ref=%s class=%s\n", sb.Ref, sb.EnforcedClass)

	teardown := func() {
		if *keep {
			fmt.Printf("keeping sandbox %s (use 'docker rm -f' to clean up)\n", sb.Ref)
			return
		}
		tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer tcancel()
		if err := run.KillSandbox(tctx, sb.Ref); err != nil {
			fmt.Fprintln(os.Stderr, "teardown:", err)
		} else {
			fmt.Println("torn down")
		}
	}
	defer teardown()

	if *execArg != "" {
		argv := strings.Fields(*execArg)
		if _, err := run.Exec(ctx, sb.Ref, argv); err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		fmt.Printf("exec started: %v\n", argv)
	}

	st, err := run.Status(ctx, sb.Ref)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	fmt.Printf("status: state=%s message=%q\n", st.State, st.Message)
	return nil
}

func loadSpec(path string) (runner.SandboxSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return runner.SandboxSpec{}, fmt.Errorf("read spec: %w", err)
	}
	var fs fileSpec
	if err := json.Unmarshal(b, &fs); err != nil {
		return runner.SandboxSpec{}, fmt.Errorf("parse spec: %w", err)
	}
	if fs.Image == "" {
		return runner.SandboxSpec{}, fmt.Errorf("spec.image is required")
	}
	runID := uuid.New()
	if fs.RunID != "" {
		id, err := uuid.Parse(fs.RunID)
		if err != nil {
			return runner.SandboxSpec{}, fmt.Errorf("parse spec.run_id: %w", err)
		}
		runID = id
	}
	return runner.SandboxSpec{
		RunID:            runID,
		Image:            fs.Image,
		ConfinementClass: fs.ConfinementClass,
		Env:              fs.Env,
		Resources: runner.Resources{
			CPUMillis: fs.Resources.CPUMillis,
			MemoryMiB: fs.Resources.MemoryMiB,
			DiskMiB:   fs.Resources.DiskMiB,
		},
		Labels: fs.Labels,
	}, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
