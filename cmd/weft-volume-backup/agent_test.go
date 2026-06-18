// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-volumes/oci/registry"
	weftvolumebackup "github.com/openweft/weft-volume-backup"
)

const goodConfig = `{
  "registry": "https://registry.weft.local",
  "username": "u", "password": "p",
  "tick": "30s",
  "targets": [{"volume":"vol-a","repo":"vols/a","interval":"1h","keepLast":3,"keepDaily":7}]
}`

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig([]byte(goodConfig))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Registry == "" || len(cfg.Targets) != 1 {
		t.Fatalf("parsed wrong: %+v", cfg)
	}
	bad := []string{
		`{`, // bad json
		`{"targets":[{"volume":"v","repo":"r","interval":"1h"}]}`, // no registry
		`{"registry":"x"}`, // no targets
		`{"registry":"x","targets":[{"repo":"r","interval":"1h"}]}`,                            // missing volume
		`{"registry":"x","targets":[{"volume":"v","interval":"1h"}]}`,                          // missing repo
		`{"registry":"x","targets":[{"volume":"v","repo":"r","interval":"nope"}]}`,             // bad interval
		`{"registry":"x","tick":"nope","targets":[{"volume":"v","repo":"r","interval":"1h"}]}`, // bad tick
	}
	for i, b := range bad {
		if _, err := parseConfig([]byte(b)); err == nil {
			t.Errorf("case %d: expected error for %s", i, b)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	if _, err := loadConfig(""); err == nil {
		t.Error("empty path should error")
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("missing file should error")
	}
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(goodConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(p); err != nil {
		t.Fatalf("loadConfig valid: %v", err)
	}
}

func TestTargetsAndTickAndClient(t *testing.T) {
	cfg, _ := parseConfig([]byte(goodConfig))
	tgs := targetsFromConfig(cfg)
	if len(tgs) != 1 || tgs[0].Volume != "vol-a" || tgs[0].Policy.KeepLast != 3 || tgs[0].Policy.Interval != time.Hour {
		t.Fatalf("targetsFromConfig wrong: %+v", tgs)
	}
	if tickOf(cfg) != 30*time.Second {
		t.Fatalf("tickOf set: %v", tickOf(cfg))
	}
	if tickOf(config{}) != defaultTick {
		t.Fatalf("tickOf default: %v", tickOf(config{}))
	}
	cl := clientForConfig(cfg)("vols/a")
	if cl.BaseURL != cfg.Registry || cl.Repository != "vols/a" || cl.Username != "u" {
		t.Fatalf("clientForConfig wrong: %+v", cl)
	}
}

func TestBuildControllerAndGCOnly(t *testing.T) {
	cfg, _ := parseConfig([]byte(goodConfig))

	// Default: no volumeAccess → GC-only snapshotter.
	if buildController(cfg, nil) == nil {
		t.Fatal("buildController (gc-only) returned nil")
	}
	// gcOnlySnapshotter takes no snapshot.
	if tag, err := (gcOnlySnapshotter{}).Snapshot(context.Background(), weftvolumebackup.Target{}); err != nil || tag != "" {
		t.Fatalf("gcOnlySnapshotter = %q,%v; want empty,nil", tag, err)
	}

	// With a volumeAccess wired → the OCI snapshotter branch.
	volumeAccess = func(context.Context, weftvolumebackup.Target) (weftvolumebackup.Committer, func(), error) {
		return weftvolumebackup.CommitterFunc(func(context.Context, *registry.Client, string) (string, error) { return "", nil }), nil, nil
	}
	t.Cleanup(func() { volumeAccess = nil })
	if buildController(cfg, nil) == nil {
		t.Fatal("buildController (with access) returned nil")
	}
}

func TestRunAgentCancelled(t *testing.T) {
	cfg, _ := parseConfig([]byte(goodConfig))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled → Run returns ctx.Err() → runAgent maps it to nil
	if err := runAgent(ctx, cfg, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("runAgent on cancel = %v; want nil", err)
	}
}

func TestCmdConstructors(t *testing.T) {
	root := newRootCmd()
	if root == nil || root.Use != "weft-volume-backup" {
		t.Fatal("newRootCmd")
	}
	// agent with no --config errors out (RunE → loadConfig("")).
	root.SetArgs([]string{"agent"})
	if err := root.Execute(); err == nil {
		t.Fatal("agent without --config should error")
	}
}

// TestAgentCmdBadConfigFile satisfies the required --config flag (so RunE runs)
// but points it at a missing file, covering RunE's loadConfig error path.
func TestAgentCmdBadConfigFile(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"agent", "--config", filepath.Join(t.TempDir(), "nope.json")})
	if err := root.Execute(); err == nil {
		t.Fatal("agent with missing config file should error")
	}
}

// fakeRunner lets a test drive runAgent's outcome without a live registry.
type fakeRunner struct{ err error }

func (f fakeRunner) Run(context.Context) error { return f.err }

// TestAgentCmdRunCancelled exercises the agent RunE end-to-end (config load →
// signal context → runAgent) with an already-cancelled context, so the control
// loop returns immediately and the command exits cleanly.
func TestAgentCmdRunCancelled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(goodConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root.SetContext(ctx)
	root.SetArgs([]string{"agent", "--config", p})
	if err := root.Execute(); err != nil {
		t.Fatalf("agent (cancelled ctx) = %v; want nil", err)
	}
}

// TestRunAgentRunError covers the non-context failure path: a runner that fails
// for a reason other than cancellation propagates the error.
func TestRunAgentRunError(t *testing.T) {
	old := buildRunner
	t.Cleanup(func() { buildRunner = old })
	boom := errors.New("run boom")
	buildRunner = func(config, *slog.Logger) controllerRunner { return fakeRunner{err: boom} }

	cfg, _ := parseConfig([]byte(goodConfig))
	if err := runAgent(context.Background(), cfg, slog.New(slog.DiscardHandler)); !errors.Is(err, boom) {
		t.Fatalf("runAgent = %v; want %v", err, boom)
	}
}

// TestRunAndMain covers run()'s success/failure exit codes and main()'s wiring
// (via the osExit seam).
func TestRunAndMain(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })

	os.Args = []string{"weft-volume-backup"} // no subcommand → help, exit 0
	if code := run(); code != 0 {
		t.Fatalf("run() no-args = %d; want 0", code)
	}
	os.Args = []string{"weft-volume-backup", "agent"} // missing --config → exit 1
	if code := run(); code != 1 {
		t.Fatalf("run() missing-config = %d; want 1", code)
	}

	var got int
	oldExit := osExit
	t.Cleanup(func() { osExit = oldExit })
	osExit = func(c int) { got = c }
	os.Args = []string{"weft-volume-backup"}
	main()
	if got != 0 {
		t.Fatalf("main exit = %d; want 0", got)
	}
}
