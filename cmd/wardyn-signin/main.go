// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-signin is the desktop image's sign-in helper (issue #2972).
// wardynd runs each subcommand with docker exec, as the uid the subcommand
// names, and every subcommand checks that uid before it does anything.
//
// The person (uid 1001) signs in to websites in a browser of their own whose
// profile, display sockets and proxy credential all live in the person's
// store: a 0700 tmpfs at /run/wardyn/signin that dies with the container. The
// agent (uid 1000) cannot read the store, cannot signal the person's
// processes, and is stopped for the whole sign-in. A login reaches the agent
// only when the person shares one site, which copies that site's cookies into
// the agent's ~/.wardyn/logins/<site>.json. That is a uid boundary inside one
// sandbox, not a sandbox boundary.
//
//	person: start | serve | hold | stop | export --site S | cdp METHOD [PARAMS]
//	agent:  pause-agent | resume-agent | import --site S | browser [URL] | cdp --agent METHOD [PARAMS]
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "start":
		err = cmdStart(os.Stdin)
	case "serve":
		err = cmdServe()
	case "hold":
		err = cmdHold()
	case "stop":
		err = cmdStop()
	case "export":
		err = cmdExport(args, os.Stdout)
	case "cdp":
		err = cmdCDP(args, os.Stdout)
	case "pause-agent":
		err = cmdPauseAgent(os.Stdout)
	case "resume-agent":
		err = cmdResumeAgent(os.Stdin)
	case "import":
		err = cmdImport(args, os.Stdin)
	case "browser":
		err = cmdBrowser(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-signin:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: wardyn-signin start|serve|hold|stop|export|cdp|pause-agent|resume-agent|import|browser")
	os.Exit(2)
}
