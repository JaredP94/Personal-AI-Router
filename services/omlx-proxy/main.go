// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/nodeid"
	"nvpair-shared/telemetry"
)

func main() {
	port := flag.Int("port", defaultProxyPort, "HTTP listen port")
	ignorePersistedPort := flag.Bool("ignore-persisted-port", false, "use --port even when a persisted port exists")
	ipcPath := flag.String("ipc", "", "IPC endpoint: Unix domain socket path or Windows named pipe (default: stdin/stdout)")
	clusterDir := flag.String("cluster-dir", "", "cluster trust directory (node.crt/key + trusted pins); enables the LAN mTLS inference ingress when this node is clustered")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	applog.Init("omlx-proxy", resolveLevel())

	var transport io.ReadWriteCloser
	if *ipcPath != "" {
		conn, err := dialIPC(*ipcPath)
		if err != nil {
			log.Fatalf("failed to connect to IPC endpoint %q: %v", *ipcPath, err)
		}
		transport = conn
		log.Printf("using IPC transport: %s", *ipcPath)
	} else {
		transport = newStdioTransport()
		log.Print("using stdio transport")
	}
	defer transport.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("received %s, shutting down", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	// Restore a previously chosen port (set via set-port) over the
	// --port/default, so the proxy comes back up where the user last put it.
	persisted, hasPersisted := loadPersistedPort()
	effectivePort := chooseStartupPort(*port, *ignorePersistedPort, persisted, hasPersisted)
	if hasPersisted && !*ignorePersistedPort && effectivePort == persisted {
		log.Printf("restored persisted proxy port %d", persisted)
	}

	tel := telemetry.NewManager("nvpair-omlx-proxy", telemetry.OTLPExporter)

	codec := NewCodec(transport)
	disc := NewDiscovery()
	proxy := NewProxy(codec, disc, effectivePort, tel)
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		_ = tel.Shutdown(shutdownCtx)
	}()
	// Open a live view of this node's cluster mTLS trust fabric. While unclustered
	// the proxy serves only the loopback plaintext personality; once this node is
	// a member the same listener also serves the pin-gated LAN mTLS ingress, and
	// peers become routable candidates. The proxy needs no restart to notice
	// either transition — it re-derives membership per request and on a watch.
	proxy.mesh = clustertrust.Open(*clusterDir)
	go proxy.mesh.Watch(ctx, func(clustered bool) {
		slog.Info("cluster inference ingress switched personality", "cluster_ingress", clustered)
		proxy.dropUnpinnedPeerTransports()
	})
	// Resolve this node's stable per-host id from the same on-disk identity
	// the broker and the scanner use, so a live frame this proxy publishes
	// names the node that is serving the request with the id the rest of
	// the cluster knows it by. The base is the parent of the cluster dir
	// (which is <base>/cluster), exactly as the broker resolves it.
	base := ""
	if *clusterDir != "" {
		base = filepath.Dir(*clusterDir)
	}
	proxy.nodeID = nodeid.Resolve(base)
	if proxy.nodeID == "" {
		slog.Warn("could not resolve this node's id; a live frame will not name the serving node")
	}

	if err := proxy.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("proxy error: %v", err)
	}
	log.Print("shutdown complete")
}
