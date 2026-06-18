// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

// Command weft-volume-backup runs the volume-backup controller as a long-lived
// agent: it loads backup Targets from a config file, builds an OCI-backed
// Registry + Snapshotter against the cluster registry, gates execution behind a
// Leader, and drives the retention control loop until interrupted.
package main

import (
	"fmt"
	"os"
)

// osExit is os.Exit behind a var so a test can observe main's exit code.
var osExit = os.Exit

func main() {
	osExit(run())
}

// run executes the root command and maps the outcome to a process exit code.
func run() int {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "weft-volume-backup:", err)
		return 1
	}
	return 0
}
