// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

const protocolV2Deadline = 25 * time.Second

// These tests catch removal of broker client-input validation, notification
// stamping, or the unstamped resync reply. Do not use startBroker: TestMain's
// binary directory contains optional engines/proxies that must not be spawned.
type protocolV2Broker struct {
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	messages     chan jsonrpc.Message
	readError    chan error
	done         chan struct{}
	stderr       bytes.Buffer // os/exec drains stderr; read only after done closes
	waitError    error
	epoch        string
	seq          uint64
	announcement *jsonrpc.Message
	heartbeat    bool
}

func startProtocolV2Broker(t *testing.T) *protocolV2Broker {
	t.Helper()
	p := &protocolV2Broker{
		cmd:      exec.Command(brokerBin, "--scanner-path", scannerBin, "--cluster-dir", t.TempDir()),
		messages: make(chan jsonrpc.Message, 64), readError: make(chan error, 1), done: make(chan struct{}),
	}
	// Optional binaries resolve ONLY against cwd, not next to brokerBin.
	p.cmd.Dir = t.TempDir()
	p.cmd.Env = os.Environ()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA"} {
		p.cmd.Env = append(p.cmd.Env, key+"="+t.TempDir())
	}
	p.cmd.Stderr = &p.stderr
	p.cmd.WaitDelay = time.Second
	var err error
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal("create isolated broker input pipe")
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		p.stdin.Close()
		t.Fatal("create isolated broker output pipe")
	}
	if err := p.cmd.Start(); err != nil {
		p.stdin.Close()
		stdout.Close()
		t.Fatal("start isolated broker")
	}
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		defer close(p.messages)
		var readErr error
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			var msg jsonrpc.Message
			if json.Unmarshal(scanner.Bytes(), &msg) != nil {
				readErr = errors.New("invalid JSON on broker output")
				continue
			}
			select {
			case p.messages <- msg:
			default:
				// Never block the child pipe, even if a regression floods output.
				readErr = errors.New("broker output exceeded test queue capacity")
			}
		}
		if scanner.Err() != nil {
			readErr = errors.New("read broker output failed")
		}
		p.readError <- readErr
	}()
	go func() {
		// Finish reading StdoutPipe before Wait closes it; Wait also joins the
		// stderr copier so inspection of stderr after done is race-free.
		<-outputDone
		p.waitError = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			// Only the process started by this helper is eligible for killing.
			p.cmd.Process.Kill()
			stdout.Close() // unblock the reader even if a descendant retained the pipe
			<-p.done
			t.Error("isolated broker exceeded shutdown deadline; killed and reaped")
		}
		if err := <-p.readError; err != nil {
			t.Error(err)
		}
		if p.waitError != nil {
			t.Error("isolated broker did not exit successfully")
		}
	})
	p.until(t, func(msg jsonrpc.Message) bool { return msg.Method == "app:ready" })
	return p
}

func (p *protocolV2Broker) send(t *testing.T, frames string) {
	t.Helper()
	if _, err := io.WriteString(p.stdin, frames+"\n"); err != nil {
		t.Fatal("write isolated broker input")
	}
}

func (p *protocolV2Broker) until(t *testing.T, accept func(jsonrpc.Message) bool) jsonrpc.Message {
	t.Helper()
	timer := time.NewTimer(protocolV2Deadline)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-p.messages:
			if !ok {
				t.Fatal("broker output EOF before expected frame")
			}
			if msg.JSONRPC != "2.0" {
				t.Fatal("broker output has invalid JSON-RPC version")
			}
			if msg.IsNotification() {
				if msg.Epoch == nil || *msg.Epoch == "" || msg.Seq == nil || *msg.Seq != p.seq+1 {
					t.Fatal("broker notification missing epoch or consecutive sequence")
				}
				if p.epoch != "" && *msg.Epoch != p.epoch {
					t.Fatal("broker notification epoch changed within process")
				}
				p.epoch, p.seq = *msg.Epoch, *msg.Seq
				if msg.Method == "broker:epoch" {
					p.announcement = &msg
				}
				if msg.Method == "heartbeat" {
					p.heartbeat = true
				}
			} else if msg.Epoch != nil || msg.Seq != nil {
				t.Fatal("broker stamped a non-notification")
			}
			if accept(msg) {
				return msg
			}
		case <-timer.C:
			t.Fatal("deadline waiting for isolated broker output")
		}
	}
}

func (p *protocolV2Broker) response(t *testing.T, id string) jsonrpc.Message {
	t.Helper()
	msg := p.until(t, func(msg jsonrpc.Message) bool {
		return msg.ID != nil && string(*msg.ID) == id && msg.Method == ""
	})
	if msg.Error != nil {
		t.Fatal("broker returned an unexpected RPC error")
	}
	return msg
}

func (p *protocolV2Broker) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(protocolV2Deadline):
		t.Fatal("deadline waiting for isolated broker exit")
	}
}

func TestProtocolV2RejectsInvalidEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"unstamped-notification", `{"jsonrpc":"2.0","method":"log/set-level","params":{"level":"info"}}`},
		{"notification-without-seq", `{"jsonrpc":"2.0","method":"log/set-level","params":{"level":"info"},"epoch":"test-client"}`},
		{"notification-without-epoch", `{"jsonrpc":"2.0","method":"log/set-level","params":{"level":"info"},"seq":1}`},
		{"stamped-request", `{"jsonrpc":"2.0","id":10,"method":"ping","epoch":"test-client","seq":1}`},
		{"stamped-response", `{"jsonrpc":"2.0","id":10,"result":{},"epoch":"test-client","seq":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startProtocolV2Broker(t)
			p.send(t, tc.frame+"\n"+`{"jsonrpc":"2.0","id":11,"method":"ping"}`)
			// Do NOT close stdin: only terminal mismatch handling may produce
			// EOF here. A reader that logs and continues would answer the ping.
			timer := time.NewTimer(protocolV2Deadline)
			defer timer.Stop()
			for {
				select {
				case msg, ok := <-p.messages:
					if !ok {
						p.waitExit(t)
						if !strings.Contains(p.stderr.String(), "JSON-RPC protocol mismatch") {
							t.Fatal("broker closed without named protocol mismatch diagnostic")
						}
						return
					}
					if msg.ID != nil {
						t.Fatal("broker answered a request after invalid envelope")
					}
				case <-timer.C:
					t.Fatal("broker did not close output on protocol mismatch with input still open")
				}
			}
		})
	}
}

func TestProtocolV2AcceptsStampedNotificationsAndUnstampedResync(t *testing.T) {
	p := startProtocolV2Broker(t)
	// The second notification deliberately skips a seq: envelope presence is
	// mandatory, but seq-gap recovery is not part of this protocol release.
	p.send(t, `{"jsonrpc":"2.0","method":"log/set-level","params":{"level":"info"},"epoch":"test-client","seq":1}`+"\n"+
		`{"jsonrpc":"2.0","method":"log/set-level","params":{"level":"info"},"epoch":"test-client","seq":3}`+"\n"+
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	ping := p.response(t, "1")
	var pong struct {
		Pong bool `json:"pong"`
	}
	if json.Unmarshal(ping.Result, &pong) != nil || !pong.Pong {
		t.Fatal("valid notifications were not followed by a successful ping")
	}
	p.send(t, `{"jsonrpc":"2.0","id":2,"method":"broker:resync"}`)
	resync := p.response(t, "2")
	var result struct {
		Epoch    string `json:"epoch"`
		Protocol int    `json:"protocol"`
	}
	if json.Unmarshal(resync.Result, &result) != nil || result.Epoch != p.epoch || result.Protocol != 2 {
		t.Fatal("resync did not return notification epoch and protocol 2")
	}
	// First real heartbeat tick also announces broker:epoch. Await channels,
	// not a sleep; allow scheduling headroom beyond the 15-second interval.
	if p.announcement == nil {
		p.until(t, func(msg jsonrpc.Message) bool { return msg.Method == "broker:epoch" })
	}
	var announced struct {
		Epoch    string `json:"epoch"`
		Protocol int    `json:"protocol"`
	}
	if json.Unmarshal(p.announcement.Params, &announced) != nil || announced != result {
		t.Fatal("broker epoch announcement disagrees with resync")
	}
	if !p.heartbeat {
		p.until(t, func(msg jsonrpc.Message) bool { return msg.Method == "heartbeat" })
	}
	p.stdin.Close()
	p.waitExit(t)
	if strings.Contains(p.stderr.String(), "JSON-RPC protocol mismatch") {
		t.Fatal("broker reported a mismatch for valid frames")
	}
	if strings.Count(p.stderr.String(), "[nvpair-ui-broker] INFO log level changed") != 2 {
		t.Fatal("valid stamped log-level notifications were not both applied")
	}
}
