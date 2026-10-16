// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command nvpair-tui is a terminal UI that spawns and supervises nvpair-ui-broker
// (and, through it, the whole NVPAIR subprocess fleet) over a stdio JSON-RPC
// connection. It is designed to run comfortably over SSH on a headless
// server where the bundled graphical UI cannot run.
//
// This file is the process entrypoint: it parses flags, initialises
// logging, spawns the broker, and drives the supervisor. Logging goes to
// stderr so it never collides with the full-screen TUI on stdout.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"nvpair-tui/rpc"
	"os"
	"os/signal"
	"syscall"

	"nvpair-shared/applog"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// It mirrors the convention every other component in this repo uses so
// `nvpair-tui --version` reports the value from versions.json.
var Version = "dev"

type ownedSession interface {
	Run() error
	Shutdown()
}

// Main owns at most one reconnect per invocation, after the old process has
// been gracefully joined. Views and pending commands never cross lifetimes.
func runOwnedSessions(ctx context.Context, spawn func() (ownedSession, error)) error {
	for attempt := 0; attempt < 2; attempt++ {
		session, err := spawn()
		if err != nil {
			return err
		}
		err = session.Run()
		session.Shutdown()
		if !errors.Is(err, rpc.ErrHeartbeatTimeout) || ctx.Err() != nil {
			return err
		}
		if attempt == 1 {
			return fmt.Errorf("broker remained unresponsive after one reconnect; restart PAIR: %w", err)
		}
	}
	return nil
}

func main() {
	brokerPath := flag.String("broker-path", "", "path to nvpair-ui-broker binary (default: ./nvpair-ui-broker alongside this executable)")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	applog.Init("nvpair-tui", resolveLevel())

	resolvedBroker, err := resolveBrokerPath(*brokerPath)
	if err != nil {
		slog.Error("cannot locate broker", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := runOwnedSessions(ctx, func() (ownedSession, error) { return Spawn(ctx, resolvedBroker) }); err != nil {
		slog.Error("TUI connection failed", "err", err)
	}
	slog.Info("shutdown complete")
}
