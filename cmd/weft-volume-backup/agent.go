// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-volumes/oci/registry"
	weftvolumebackup "github.com/openweft/weft-volume-backup"
	"github.com/spf13/cobra"
)

// config is the on-disk agent configuration (JSON): the registry endpoint, the
// per-volume backup targets, and the loop cadence. Snapshotting a volume needs a
// VolumeAccess that opens it; the standalone agent ships with none (the wiring
// is the weft-driver-qemu integration point), so it runs in registry-GC-only
// mode unless built with an access provider — see runAgent.
type config struct {
	// Registry is the OCI registry base URL, e.g. "https://registry.weft.local".
	Registry string `json:"registry"`
	// Username/Password are optional registry credentials (Basic / bearer).
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Tick is how often the loop re-evaluates schedules (Go duration string).
	Tick string `json:"tick,omitempty"`
	// Targets are the volumes to back up.
	Targets []targetConfig `json:"targets"`
}

// targetConfig is one volume's backup spec in the config file.
type targetConfig struct {
	Volume      string `json:"volume"`
	Repo        string `json:"repo"`
	Interval    string `json:"interval"`
	KeepLast    int    `json:"keepLast,omitempty"`
	KeepDaily   int    `json:"keepDaily,omitempty"`
	KeepWeekly  int    `json:"keepWeekly,omitempty"`
	KeepMonthly int    `json:"keepMonthly,omitempty"`
	KeepYearly  int    `json:"keepYearly,omitempty"`
}

// defaultTick is used when config.Tick is empty.
const defaultTick = time.Minute

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "weft-volume-backup",
		Short:         "OCI snapshot + retention controller for go-volumes volumes",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newAgentCmd())
	return root
}

func newAgentCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the backup/retention control loop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log := slog.New(slog.NewTextHandler(cmd.OutOrStdout(), nil))
			return runAgent(ctx, cfg, log)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to the JSON config file (required)")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

// loadConfig reads and validates the JSON config at path.
func loadConfig(path string) (config, error) {
	if path == "" {
		return config{}, fmt.Errorf("a --config path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read config: %w", err)
	}
	return parseConfig(data)
}

// parseConfig unmarshals and validates raw config bytes.
func parseConfig(data []byte) (config, error) {
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	if strings.TrimSpace(cfg.Registry) == "" {
		return config{}, fmt.Errorf("config: registry is required")
	}
	if len(cfg.Targets) == 0 {
		return config{}, fmt.Errorf("config: at least one target is required")
	}
	for i, tc := range cfg.Targets {
		if tc.Volume == "" || tc.Repo == "" {
			return config{}, fmt.Errorf("config: target %d: volume and repo are required", i)
		}
		if _, err := time.ParseDuration(tc.Interval); err != nil {
			return config{}, fmt.Errorf("config: target %d (%s): bad interval %q: %w", i, tc.Volume, tc.Interval, err)
		}
	}
	if cfg.Tick != "" {
		if _, err := time.ParseDuration(cfg.Tick); err != nil {
			return config{}, fmt.Errorf("config: bad tick %q: %w", cfg.Tick, err)
		}
	}
	return cfg, nil
}

// clientForConfig builds the ClientFor that maps a repo to a registry client
// sharing cfg's base URL and credentials.
func clientForConfig(cfg config) weftvolumebackup.ClientFor {
	return func(repo string) *registry.Client {
		return &registry.Client{
			BaseURL:    cfg.Registry,
			Repository: repo,
			Username:   cfg.Username,
			Password:   cfg.Password,
		}
	}
}

// targetsFromConfig converts the config's targets to the controller's Target type.
func targetsFromConfig(cfg config) []weftvolumebackup.Target {
	out := make([]weftvolumebackup.Target, 0, len(cfg.Targets))
	for _, tc := range cfg.Targets {
		d, _ := time.ParseDuration(tc.Interval) // validated in parseConfig
		out = append(out, weftvolumebackup.Target{
			Volume: tc.Volume,
			Repo:   tc.Repo,
			Policy: weftvolumebackup.Policy{
				Interval:    d,
				KeepLast:    tc.KeepLast,
				KeepDaily:   tc.KeepDaily,
				KeepWeekly:  tc.KeepWeekly,
				KeepMonthly: tc.KeepMonthly,
				KeepYearly:  tc.KeepYearly,
			},
		})
	}
	return out
}

// tickOf returns the configured tick or the default.
func tickOf(cfg config) time.Duration {
	if cfg.Tick == "" {
		return defaultTick
	}
	d, _ := time.ParseDuration(cfg.Tick) // validated in parseConfig
	return d
}

// volumeAccess is the seam the standalone binary cannot fill on its own: opening
// a volume to snapshot it requires the weft-driver-qemu GoVolume driver. It is a
// package-level var so an integration build (or a test) can inject a real
// VolumeAccess; left nil, the agent runs retention-GC only and never snapshots.
var volumeAccess weftvolumebackup.VolumeAccess

// leaderProvider yields the Leader; a package-level var so an integration build
// can supply an etcd / replica-ha election. The default is single-node.
var leaderProvider = func() weftvolumebackup.Leader { return weftvolumebackup.AlwaysLeader{} }

// buildController assembles a Controller from cfg, wiring the OCI registry and
// (when volumeAccess is set) the OCI snapshotter; otherwise a no-op snapshotter
// that performs GC only.
func buildController(cfg config, log *slog.Logger) *weftvolumebackup.Controller {
	clientFor := clientForConfig(cfg)
	reg := weftvolumebackup.NewOCIRegistry(clientFor)

	var snap weftvolumebackup.Snapshotter
	if volumeAccess != nil {
		snap = weftvolumebackup.NewOCISnapshotter(volumeAccess, clientFor)
	} else {
		snap = gcOnlySnapshotter{}
	}

	return weftvolumebackup.New(
		targetsFromConfig(cfg), snap, reg, leaderProvider(), nil, tickOf(cfg), log,
	)
}

// controllerRunner is the slice of *Controller that runAgent drives. A
// package-level constructor var wraps it so a test can inject a fake runner
// (e.g. one whose Run returns a non-context error) without a live registry.
type controllerRunner interface {
	Run(ctx context.Context) error
}

var buildRunner = func(cfg config, log *slog.Logger) controllerRunner {
	return buildController(cfg, log)
}

// runAgent builds the controller and runs it until ctx is cancelled. A context
// cancellation (signal) is a clean stop, not an error; any other failure
// propagates.
func runAgent(ctx context.Context, cfg config, log *slog.Logger) error {
	log.Info("weft-volume-backup agent starting",
		"registry", cfg.Registry, "targets", len(cfg.Targets), "tick", tickOf(cfg))
	err := buildRunner(cfg, log).Run(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// gcOnlySnapshotter is the Snapshotter used when no VolumeAccess is wired: it
// takes no snapshot (returns an empty tag) so the controller still runs the
// retention/GC sweep against the existing tags. It exists so the standalone
// binary is useful for GC even without the qemu-driver integration.
type gcOnlySnapshotter struct{}

func (gcOnlySnapshotter) Snapshot(context.Context, weftvolumebackup.Target) (string, error) {
	return "", nil
}
