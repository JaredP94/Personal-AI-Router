// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process tests for the two broker-management hotfix legs:
//
//	A. the broker bridges explicitly configured bare engines into the proxies,
//	   but keeps unverified PAIR nodes as non-routable previews; and
//	B. the broker accepts a client-originated errors:report and forwards it
//	   into nvpair-errors, reflected in the next errors:update.
//
// Both reuse the supervision harness (startBrokerWith) and binaries built by
// TestMain.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

// fakeOllama stands up a minimal Ollama-looking HTTP server on the fixed
// port nvpair-manual-nodes probes (11434): GET / returns 200 (liveness) and
// GET /api/tags returns one model, so a manual node pointed at it reports
// ollama_up=true with a model list. Returns a stop func. Skips the test if
// the port can't be bound (real Ollama or something else holds it — we
// can't control the response then).
func fakeOllama(t *testing.T) func() {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:11434")
	if err != nil {
		t.Skipf("cannot bind fake ollama on 127.0.0.1:11434 (%v); skipping", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.2:latest"}]}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

// fakeLMStudio stands up a minimal LM Studio-looking OpenAI-API server on the
// fixed port nvpair-manual-nodes probes (1234): GET /v1/models returns 200 with a
// one-model list, so a manual node pointed at it reports lmstudio_up=true.
// Skips the test if the port can't be bound.
func fakeLMStudio(t *testing.T) func() {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:1234")
	if err != nil {
		t.Skipf("cannot bind fake lmstudio on 127.0.0.1:1234 (%v); skipping", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"qwen2.5-7b-instruct","object":"model"}]}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

func persistLMStudioProxyPort(t *testing.T, port int) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("APPDATA", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("HOME", root)
	path, err := appdir.Path("lmstudio-proxy-port.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"port":%d}`, port)), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// fakeNodeInfo stands up a minimal node-info server on the fixed port the
// manual prober hits (14318). A non-nil hostUUID serves an unclustered PAIR
// preview, never an identity proof. Nil returns 404 while still holding the
// port, so a real local PAIR cannot answer for a bare-engine fixture.
// Skips the test if the port can't be bound.
func fakeNodeInfo(t *testing.T, hostUUID *string) func() {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:14318")
	if err != nil {
		t.Skipf("cannot bind fake node-info on 127.0.0.1:14318 (%v); skipping", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", func(w http.ResponseWriter, r *http.Request) {
		if hostUUID == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"GPUs":[],"hostUuid":%q}`, *hostUUID))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

// proxyNode mirrors the proxy's nodes/list element (ollama-proxy Node).
type proxyNode struct {
	ID        string   `json:"id"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	TXT       []string `json:"txt"`
}

type proxyNodesResult struct {
	Nodes []proxyNode `json:"nodes"`
}

func proxyNodesHas(t *testing.T, raw json.RawMessage, id string) bool {
	t.Helper()
	var res proxyNodesResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode proxy nodes/list: %v", err)
	}
	for _, n := range res.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

type manualInventoryNode struct {
	ID                   string   `json:"id"`
	HostUUID             string   `json:"hostUuid"`
	PeerRecordID         string   `json:"peerRecordId"`
	IdentityVerification string   `json:"identityVerification"`
	Trusted              bool     `json:"trusted"`
	Models               []string `json:"models"`
}

func brokerNodesResult(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, method string) json.RawMessage {
	t.Helper()
	msg := callRPC(t, stdin, msgs, method, nil, 5*time.Second)
	if msg.Error != nil {
		t.Fatalf("%s failed: %+v", method, msg.Error)
	}
	return msg.Result
}

func manualInventory(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, nodeName string) (manualInventoryNode, bool) {
	t.Helper()
	var res struct {
		Nodes []manualInventoryNode `json:"nodes"`
	}
	if err := json.Unmarshal(brokerNodesResult(t, stdin, msgs, "discovery:get-nodes"), &res); err != nil {
		t.Fatalf("decode discovery:get-nodes: %v", err)
	}
	for _, n := range res.Nodes {
		if n.ID == nodeName {
			return n, true
		}
	}
	return manualInventoryNode{}, false
}

func assertManualInventory(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, nodeName, verification string) {
	t.Helper()
	n, ok := manualInventory(t, stdin, msgs, nodeName)
	if !ok || n.IdentityVerification != verification || n.PeerRecordID != noderec.ManualRecordID(nodeName) || n.Trusted {
		t.Fatalf("manual inventory = %+v (present=%v), want independent %s record without peer trust", n, ok, verification)
	}
}

// An explicitly configured bare Ollama remains a labelled manual routing bypass,
// even though it never appears over mDNS and supplies no PAIR identity proof.
func TestBrokerBridgesBareManualOllamaIntoProxy(t *testing.T) {
	if portBusy(11435) {
		t.Skip("ollama-proxy port 11435 already in use; skipping")
	}
	stopOllama := fakeOllama(t) // skips if 11434 unavailable
	t.Cleanup(stopOllama)

	// Hold node-info without answering as PAIR, preventing a real local PAIR
	// from turning this bare engine into an unverified peer preview.
	t.Cleanup(fakeNodeInfo(t, nil))

	stdin, msgs, _, cleanup := startBrokerWith(t,
		"--manual-nodes-path", manualNodesBin,
		"--proxy-path", proxyBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const nodeName = "xproc-manual-proxy-bridge"
	addReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":900,"method":"node/add","params":{"address":"127.0.0.1","name":%q}}`, nodeName) + "\n"
	if _, err := stdin.Write([]byte(addReq)); err != nil {
		t.Fatalf("write node/add: %v", err)
	}

	// Poll the proxy's node set (via the broker's proxy: relay) until the
	// bridged manual node appears. The first probe fires immediately on
	// node/add; re-probes every 10s re-bridge if the proxy wasn't ready yet.
	deadline := time.After(25 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	reqID := 901
	sendReq(t, stdin, reqID, "proxy:nodes/list")
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before the manual node bridged into the proxy")
			}
			if msg.Method == "" && msg.ID != nil {
				if msg.Error != nil {
					t.Fatalf("broker request failed: %+v", msg.Error)
				}
				if proxyNodesHas(t, msg.Result, noderec.ManualRecordID(nodeName)) {
					assertManualInventory(t, stdin, msgs, nodeName, "manual")
					t.Logf("bare manual Ollama %q bridged into proxy nodes/list", nodeName)
					return
				}
			}
		case <-ticker.C:
			reqID++
			sendReq(t, stdin, reqID, "proxy:nodes/list")
		case <-deadline:
			t.Fatalf("timed out waiting for manual node %q in proxy:nodes/list", nodeName)
		}
	}
}

// Public PAIR presence denies the bare-engine bypass, with or without a claimed
// UUID. Neither the manual alias, preview record nor claimed UUID may route.
// Verified UUID keying is covered by TestManualToEnrichedHostUUID and
// TestManualNodeBridgesToRelayDir in nvpair-ui-broker; this unclustered fixture
// deliberately cannot supply a pinned mTLS proof or authenticated metadata.
func TestBrokerKeepsUnclusteredManualPAIRAsUnverifiedPreview(t *testing.T) {
	if portBusy(11435) {
		t.Skip("ollama-proxy port 11435 already in use; skipping")
	}
	t.Cleanup(fakeOllama(t))
	t.Cleanup(fakeLMStudio(t))

	for _, tc := range []struct {
		name     string
		hostUUID string
	}{
		{name: "no-uuid"},
		{name: "claimed-uuid", hostUUID: "unverified-claimed-host-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := persistLMStudioProxyPort(t, freePort(t))
			t.Cleanup(fakeNodeInfo(t, &tc.hostUUID))
			stdin, msgs, _, cleanup := startBrokerWithConfigDir(t, configDir,
				"--manual-nodes-path", manualNodesBin,
				"--proxy-path", proxyBin,
				"--lmstudio-proxy-path", lmstudioProxyBin,
			)
			t.Cleanup(cleanup)
			waitForMethod(t, msgs, "app:ready", 10*time.Second)

			nodeName := "xproc-manual-pair-" + tc.name
			added := callRPC(t, stdin, msgs, "node/add", map[string]string{"address": "127.0.0.1", "name": nodeName}, 5*time.Second)
			if added.Error != nil {
				t.Fatalf("node/add failed: %+v", added.Error)
			}

			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			var previewSince time.Time
			for {
				n, ok := manualInventory(t, stdin, msgs, nodeName)
				if ok && n.IdentityVerification == "unverified" {
					if n.PeerRecordID != noderec.ManualRecordID(nodeName) || n.Trusted || len(n.Models) != 0 {
						t.Fatalf("PAIR preview acquired peer trust or routing inventory: %+v", n)
					}
					if tc.hostUUID != "" && (n.HostUUID == tc.hostUUID || n.PeerRecordID == tc.hostUUID) {
						t.Fatalf("unverified UUID became the preview identity: %+v", n)
					}
					if previewSince.IsZero() {
						previewSince = time.Now()
					}
				} else if !previewSince.IsZero() {
					t.Fatalf("unverified preview disappeared or changed kind: %+v (present=%v)", n, ok)
				}

				for _, method := range []string{"proxy:nodes/list", "lmstudio-proxy:nodes/list"} {
					raw := brokerNodesResult(t, stdin, msgs, method)
					for _, key := range []string{nodeName, noderec.ManualRecordID(nodeName), tc.hostUUID} {
						if key != "" && proxyNodesHas(t, raw, key) {
							t.Fatalf("unverified PAIR node routed as %q in %s", key, method)
						}
					}
				}
				// Observe a full 10s manual-worker re-probe after the preview
				// appears, rather than passing on an empty startup snapshot.
				if !previewSince.IsZero() && time.Since(previewSince) >= 12*time.Second {
					return
				}
				select {
				case <-ticker.C:
				case <-deadline.C:
					t.Fatal("timed out waiting for a stable unverified PAIR preview")
				}
			}
		})
	}
}

// Bare LM Studio has the same explicit manual bypass as bare Ollama.
func TestBrokerBridgesBareManualLMStudioIntoProxy(t *testing.T) {
	configDir := persistLMStudioProxyPort(t, freePort(t))
	stopLM := fakeLMStudio(t) // skips if 1234 unavailable
	t.Cleanup(stopLM)
	t.Cleanup(fakeNodeInfo(t, nil))

	stdin, msgs, _, cleanup := startBrokerWithConfigDir(t, configDir,
		"--manual-nodes-path", manualNodesBin,
		"--lmstudio-proxy-path", lmstudioProxyBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const nodeName = "xproc-manual-lmstudio-bridge"
	addReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":920,"method":"node/add","params":{"address":"127.0.0.1","name":%q}}`, nodeName) + "\n"
	if _, err := stdin.Write([]byte(addReq)); err != nil {
		t.Fatalf("write node/add: %v", err)
	}

	deadline := time.After(25 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	reqID := 921
	sendReq(t, stdin, reqID, "lmstudio-proxy:nodes/list")
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before the manual node bridged into lmstudio-proxy")
			}
			if msg.Method == "" && msg.ID != nil {
				if msg.Error != nil {
					t.Fatalf("broker request failed: %+v", msg.Error)
				}
				if proxyNodesHas(t, msg.Result, noderec.ManualRecordID(nodeName)) {
					assertManualInventory(t, stdin, msgs, nodeName, "manual")
					t.Logf("bare manual LM Studio %q bridged into proxy nodes/list", nodeName)
					return
				}
			}
		case <-ticker.C:
			reqID++
			sendReq(t, stdin, reqID, "lmstudio-proxy:nodes/list")
		case <-deadline:
			t.Fatalf("timed out waiting for manual node %q in lmstudio-proxy:nodes/list", nodeName)
		}
	}
}

// TestBrokerAcceptsClientErrorsReport is the leg-B repro: a client-originated
// errors:report (notification form) must be forwarded into nvpair-errors and
// reflected in the next errors:update — the same as a supervised worker
// emitting one on its stdout.
func TestBrokerAcceptsClientErrorsReport(t *testing.T) {
	if portBusy(14319) {
		t.Skip("nvpair-errors --peer-sync port 14319 already in use; skipping")
	}

	stdin, msgs, _, cleanup := startBrokerWith(t, "--errors-path", errorsBin)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const id = "client:leg-b-notif"
	report := fmt.Sprintf(`{"jsonrpc":"2.0","method":"errors:report","params":{"id":%q,"message":"client-synthesized","severity":"error"}}`, id)
	nextSeq := 1
	writeStampedRawNotification(t, stdin, &nextSeq, report)

	// The broker forwards it into nvpair-errors, which stores it and pushes an
	// errors:update the broker relays back unconditionally.
	waitForErrorsUpdateContaining(t, msgs, id, 10*time.Second)
	t.Logf("client errors:report %q reflected in errors:update", id)
}

// TestBrokerAcceptsClientErrorsReportRequest covers the request form of
// leg B: an id-bearing errors:report is acked with a null result and also
// reflected in errors:update.
func TestBrokerAcceptsClientErrorsReportRequest(t *testing.T) {
	if portBusy(14319) {
		t.Skip("nvpair-errors --peer-sync port 14319 already in use; skipping")
	}

	stdin, msgs, _, cleanup := startBrokerWith(t, "--errors-path", errorsBin)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const (
		reqID = 950
		id    = "client:leg-b-req"
	)
	report := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"errors:report","params":{"id":%q,"message":"client-synthesized","severity":"error"}}`, reqID, id) + "\n"
	if _, err := stdin.Write([]byte(report)); err != nil {
		t.Fatalf("write errors:report request: %v", err)
	}

	// Expect both the ack (result for reqID, no error) and the errors:update.
	gotAck := false
	gotUpdate := false
	deadline := time.After(10 * time.Second)
	for !(gotAck && gotUpdate) {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before the report was acked and reflected")
			}
			switch {
			case msg.Method == "" && msg.ID != nil:
				if string(*msg.ID) == fmt.Sprintf("%d", reqID) {
					if msg.Error != nil {
						t.Fatalf("errors:report request returned an error: %+v", msg.Error)
					}
					gotAck = true
				}
			case msg.Method == methodErrorsUpdate:
				if errorsListHasID(t, msg.Params, id) {
					gotUpdate = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out (ack=%v update=%v) for errors:report request %q", gotAck, gotUpdate, id)
		}
	}
}
