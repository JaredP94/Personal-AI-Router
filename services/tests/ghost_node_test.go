// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

// TestScannerEvictsRecordSupersededAtItsAddress is the cross-process regression
// test for the duplicate ("ghost") verified node a reset machine leaves behind.
// Pinned mTLS inventory refresh, not the public browser's TCP guard, owns this
// record's lifetime. A wiped machine loses the old key and cluster admission,
// but still answers public node-info under its fresh identity on the same port.
func TestScannerEvictsRecordSupersededAtItsAddress(t *testing.T) {
	requireMulticast(t)
	const (
		originalUUID = "11111111-aaaa-4aaa-8aaa-000000000001"
		wipedUUID    = "22222222-bbbb-4bbb-8bbb-000000000002"
	)
	fx := newInterNodeClusterWithPeer(t, originalUUID)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	stop := fx.startVerifiedPeerOn(t, "ghost-verified-peer", nil, nil, ln)
	events := startScannerForGhostTest(t, fx.nodeDir)
	original, ok := awaitNodeEvent(events, noderec.NotifyNodeUpdated, originalUUID, 30*time.Second)
	if !ok {
		t.Fatalf("verified peer record was never discovered after requireMulticast succeeded")
	}
	if !original.ServicePeer() || original.IdentityVerification != "verified" || original.PeerRecordID == "" {
		t.Fatalf("fixture did not prove the original peer: %+v", original)
	}

	// Drop the old advertisement and key-bearing listener, then bring up the
	// wiped, unclustered machine at the identical address. A live public HTTP
	// response proves this test cannot pass merely because a TCP port closed.
	stop()
	replacement, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("rebind wiped peer at %s: %v", addr, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hostUuid": wipedUUID, "clusterUuid": "", "GPUs": []any{}})
	})}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve(replacement) }()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/v1/node-info")
	if err != nil {
		t.Fatalf("wiped peer must still answer at the original endpoint: %v", err)
	}
	var info struct {
		HostUUID string `json:"hostUuid"`
	}
	err = json.NewDecoder(resp.Body).Decode(&info)
	_ = resp.Body.Close()
	if err != nil || info.HostUUID != wipedUUID {
		t.Fatalf("wiped identity = %q, decode error = %v", info.HostUUID, err)
	}
	removed, ok := awaitNodeEvent(events, noderec.NotifyNodeRemoved, originalUUID, 2*time.Minute)
	if !ok {
		t.Fatalf("scanner kept %s after a fresh identity answered at its address", originalUUID)
	}
	if removed.PeerRecordID != original.PeerRecordID || removed.IdentityVerification != "verified" {
		t.Fatalf("withdrawal did not identify the original verified record: %+v", removed)
	}
}

// TestScannerEvictsPreviewSupersededAtItsAddress covers the separate public
// candidate lifetime. Unlike a verified peer, it uses the browser miss threshold
// and identity-confirming public liveness probe.
//
// A machine whose appdata is wiped mints a fresh hostUuid but keeps its LAN
// address and re-binds the same fixed service ports. The scanner's
// evict-on-mDNS-miss guard probes a missing node before dropping it, and a
// TCP-only probe reached the machine's NEW incarnation on those same ports and
// reported the OLD record alive — forever. The probe now confirms identity, so
// this exercises the whole chain in one process tree: mDNS browse -> miss
// threshold -> liveness probe -> node-info identity mismatch -> node-removed.
//
// The peer is simulated rather than a second PAIR install: a zeroconf
// responder publishes the record, and a local HTTP server plays its node-info.
// Flipping that server's reported hostUuid is exactly the signal a wipe
// produces, and is what the probe must notice.
func TestScannerEvictsPreviewSupersededAtItsAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("multicast + miss-threshold timing; skipped under -short")
	}
	requireMulticast(t)

	const (
		peerInstance = "ghost-peer-host"
		originalUUID = "11111111-aaaa-4aaa-8aaa-000000000001"
		wipedUUID    = "22222222-bbbb-4bbb-8bbb-000000000002"
	)

	// The peer's node-info. It reports originalUUID until the "wipe", after
	// which it reports wipedUUID from the same address and port — the machine is
	// up and listening throughout, which is precisely why a TCP probe cannot
	// tell the two apart.
	reported := &atomic.Value{}
	reported.Store(originalUUID)
	nodeInfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"GPUs":[],"hostUuid":"` + reported.Load().(string) + `"}`))
	}))
	defer nodeInfo.Close()

	_, niPortStr, err := net.SplitHostPort(strings.TrimPrefix(nodeInfo.URL, "http://"))
	if err != nil {
		t.Fatalf("node-info addr: %v", err)
	}
	niPort, err := strconv.Atoi(niPortStr)
	if err != nil {
		t.Fatalf("node-info port: %v", err)
	}

	// Publish the peer's _nvpair-node record pointing at that node-info.
	txt := []string{
		"v=" + noderec.SchemaVersion,
		"uuid=" + originalUUID,
		"ip=127.0.0.1",
		fmt.Sprintf("%s=%d", noderec.ServiceNodeInfo, niPort),
	}
	responder, err := zeroconf.Register(peerInstance, noderec.ServiceType, testDomain, noderec.SRVPort, txt, nil)
	if err != nil {
		t.Fatalf("register peer: %v", err)
	}
	responderDown := false
	defer func() {
		if !responderDown {
			responder.Shutdown()
		}
	}()

	events := startScannerForGhostTest(t, t.TempDir())

	// requireMulticast already made the environment decision. Discovery failure
	// here is a regression, not grounds for an inconclusive self-skip.
	preview, ok := awaitNodeEventMatching(events, noderec.NotifyNodeUpdated, func(n noderec.DirectoryNode) bool {
		return n.Name == peerInstance && n.HostUUID == originalUUID && n.IdentityVerification == "unverified"
	}, 30*time.Second)
	key := preview.RecordKey()
	if !ok {
		t.Fatalf("preview record was never discovered after requireMulticast succeeded")
	}
	if preview.IdentityVerification != "unverified" || preview.Trusted || !strings.HasPrefix(key, "preview:") || preview.PeerRecordID != key {
		t.Fatalf("plaintext fixture unexpectedly proved an identity: %+v", preview)
	}

	// The wipe: the record stops being advertised under the old identity, and
	// the machine now answers as someone else at the same address and port.
	responder.Shutdown()
	responderDown = true
	reported.Store(wipedUUID)

	// Eviction waits on the browser's full miss threshold (12 scans at 5s, a full
	// minute) — the window is sized to outlast a node saturated by its own
	// inference load — so this deadline is generous by design. The identity probe
	// runs on every scan from ~15s in, so the mismatch is detected long before the
	// eviction it eventually authorizes.
	if _, ok := awaitNodeEvent(events, noderec.NotifyNodeRemoved, key, 2*time.Minute); !ok {
		t.Fatalf("scanner never evicted %s after its address began answering as %s; "+
			"a record whose machine was re-identified must not be kept alive by a TCP probe",
			originalUUID, wipedUUID)
	}
}

// startScannerForGhostTest runs the scanner binary on stdio and returns its
// notification stream. The scanner emits discovery:node-* unconditionally, so
// no subscription is needed. stdin is held open for the process lifetime —
// closing it ends the scanner's read loop.
//
// --cluster-dir is a throwaway identity (empty or synthetically admitted), as
// everywhere else in this package.
// Without it the scanner resolves the real per-user app dir: it would read (and
// mint into) the developer's own identity files and advertise their machine's
// real cluster identity on the LAN while the test runs.
func startScannerForGhostTest(t *testing.T, clusterDir string) <-chan jsonrpc.Message {
	t.Helper()
	cmd := testCommand(t, scannerBin, "--cluster-dir", clusterDir)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("scanner stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("scanner stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start scanner: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return startMsgReader(stdout)
}

// awaitNodeEvent matches the independent record key, never a preview's claimed
// host UUID. Other events (self, enrichment, other candidates) are ignored.
func awaitNodeEvent(events <-chan jsonrpc.Message, method, recordKey string, timeout time.Duration) (noderec.DirectoryNode, bool) {
	return awaitNodeEventMatching(events, method, func(n noderec.DirectoryNode) bool { return n.RecordKey() == recordKey }, timeout)
}

func awaitNodeEventMatching(events <-chan jsonrpc.Message, method string, match func(noderec.DirectoryNode) bool, timeout time.Duration) (noderec.DirectoryNode, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-events:
			if !ok {
				return noderec.DirectoryNode{}, false
			}
			if msg.Method != method {
				continue
			}
			var ev noderec.NodeEvent
			if err := json.Unmarshal(msg.Params, &ev); err != nil {
				continue
			}
			if match(ev.Node) {
				return ev.Node, true
			}
		case <-deadline:
			return noderec.DirectoryNode{}, false
		}
	}
}
