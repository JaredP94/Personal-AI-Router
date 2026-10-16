// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package rpc

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestClientSilenceExpiresAndWakesConsumers(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	c := NewClient(cli, cli)
	c.heartbeatTimeout = 20 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	pending := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "held", nil); pending <- err }()
	codec := NewCodec(srv)
	if _, err := codec.Read(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrHeartbeatTimeout) {
			t.Fatalf("timeout identity lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("silence not detected")
	}
	select {
	case <-pending:
	case <-time.After(time.Second):
		t.Fatal("pending call not woken")
	}
	if _, ok := <-c.Notifications(); ok {
		t.Fatal("notifications not closed")
	}
	if !errors.Is(c.Err(), ErrHeartbeatTimeout) {
		t.Fatalf("consumer cannot distinguish silence: %v", c.Err())
	}
}
func TestSilenceDeadlineResetAndStaleCallbacks(t *testing.T) {
	var callbacks []func()
	expired := 0
	d := &silenceDeadline{duration: time.Second, expire: func() { expired++ }, schedule: func(_ time.Duration, f func()) func() { callbacks = append(callbacks, f); return func() {} }}
	d.reset()
	d.reset()
	callbacks[0]()
	if expired != 0 {
		t.Fatal("old timer expired refreshed connection")
	}
	callbacks[1]()
	callbacks[1]()
	if expired != 1 {
		t.Fatalf("timeout emitted %d times", expired)
	}
	d.stop()
	d.reset()
	if len(callbacks) != 2 {
		t.Fatal("closed deadline rearmed")
	}
	other := &silenceDeadline{duration: time.Second, expire: func() { expired++ }, schedule: d.schedule}
	other.reset()
	other.stop()
	callbacks[2]()
	if expired != 1 {
		t.Fatal("teardown did not cancel deadline")
	}
}

func TestClientHeartbeatsKeepLiveAndCancelStopsDeadline(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	c := NewClient(cli, cli)
	c.heartbeatTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	codec := NewCodec(srv)
	for i := 0; i < 10; i++ {
		if err := codec.Notify("heartbeat", nil); err != nil {
			t.Fatal(err)
		}
		select {
		case <-c.Notifications():
		case <-time.After(time.Second):
			t.Fatal("heartbeat not consumed")
		}
		select {
		case err := <-done:
			t.Fatalf("healthy channel declared dead: %v", err)
		case <-time.After(15 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if errors.Is(err, ErrHeartbeatTimeout) {
			t.Fatal("teardown mistaken for timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not close owned reader")
	}
}

func TestSilenceKeepsOwnedOutputDrainedUntilGracefulPeerExit(t *testing.T) {
	output, peerOutput := io.Pipe()
	input, clientInput := io.Pipe()
	defer output.Close()
	defer peerOutput.Close()
	defer input.Close()
	defer clientInput.Close()
	c := NewClient(output, clientInput)
	c.heartbeatTimeout = 20 * time.Millisecond
	peerFinished := make(chan error, 1)
	go func() {
		_, _ = io.Copy(io.Discard, input) // EOF is the graceful owned shutdown signal
		err := NewCodec(readWriter{Reader: input, Writer: peerOutput}).Notify("shutdown-complete", nil)
		peerFinished <- err
		peerOutput.Close()
	}()
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout did not invalidate session")
	}
	select {
	case err := <-peerFinished:
		if err != nil {
			t.Fatalf("closed stdout before graceful peer exit (SIGPIPE risk): %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old session output not drained during owned shutdown")
	}
	if _, ok := <-c.Notifications(); ok {
		t.Fatal("shutdown output reached invalidated UI")
	}
}
