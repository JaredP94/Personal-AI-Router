// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	settings "nvpair-shared/enginesettings"
)

func settingsMesh(t *testing.T, id string) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse("urn:nvpair:node:" + id)
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: id},
		URIs:         []*url.URL{uri},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	for name, data := range map[string][]byte{"node.crt": certPEM, "node.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, certPEM
}

func settingsPin(t *testing.T, dir, id string, cert []byte) {
	t.Helper()
	path := filepath.Join(dir, "trusted")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"nodeUuid": id, "certPem": string(cert)})
	if err := os.WriteFile(filepath.Join(path, id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsPairedMutationPushObserversAndRevocation(t *testing.T) {
	a, certA := settingsMesh(t, "a")
	b, certB := settingsMesh(t, "b")
	c, certC := settingsMesh(t, "c")
	stranger, _ := settingsMesh(t, "stranger")
	settingsPin(t, a, "b", certB)
	settingsPin(t, a, "c", certC)
	for _, dir := range []string{b, c, stranger} {
		settingsPin(t, dir, "a", certA)
	}
	mesh := clustertrust.Open(a)
	exec := settingsExecutor(t, false)
	snapshot := settings.Snapshot{
		NodeID:   "a-host",
		Engine:   "ollama",
		Revision: 1,
		Epoch:    "epoch-a",
		Sequence: 1,
		Phase:    "idle",
		Settings: settings.Config{ServerPort: 12000, ProxyPort: 12001, LaunchText: "managed serve"},
	}
	exec.settingsHub.Publish([]settings.Snapshot{snapshot})
	var mu sync.Mutex
	callerSeen := ""
	exec.settingsParent = func(ctx context.Context, method string, p settings.Request, caller string) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		callerSeen = caller
		if p.NodeID != "" {
			t.Error("forwarded another target through settings HTTP")
		}
		if method == "apply" {
			snapshot.Revision++
			snapshot.Sequence++
			snapshot.Settings = p.Settings
			exec.settingsHub.Publish([]settings.Snapshot{snapshot})
		}
		data, _ := json.Marshal(snapshot)
		return data, nil
	}
	server := httptest.NewUnstartedServer((&controlServer{exec: exec, mesh: mesh}).mux())
	server.TLS = mesh.ServerTLSConfig()
	server.StartTLS()
	defer server.Close()
	client := func(dir string) *http.Client {
		m := clustertrust.Open(dir)
		config, ok := m.ClientTLSConfig("a")
		if !ok {
			t.Fatal("pin unavailable")
		}
		tr := &http.Transport{TLSClientConfig: config}
		t.Cleanup(tr.CloseIdleConnections)
		return &http.Client{Transport: tr}
	}
	clientB, clientC := client(b), client(c)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	open := func(cl *http.Client) (*http.Response, *bufio.Scanner) {
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+settingsPath+"events", nil)
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		return res, bufio.NewScanner(res.Body)
	}
	_, scanB := open(clientB)
	resC, scanC := open(clientC)
	read := func(scan *bufio.Scanner, want uint64) {
		t.Helper()
		for scan.Scan() {
			var rows []settings.Snapshot
			if json.Unmarshal(scan.Bytes(), &rows) != nil {
				t.Fatal("invalid stream")
			}
			if len(rows) > 0 {
				if rows[0].Revision != want {
					t.Fatalf("revision=%d want=%d", rows[0].Revision, want)
				}
				return
			}
		}
		t.Fatalf("stream ended: %v", scan.Err())
	}
	read(scanB, 1)
	read(scanC, 1)
	config := settings.Config{ServerPort: 12002, ProxyPort: 12003, LaunchText: "managed serve --parallel 2"}
	body, _ := json.Marshal(settings.Request{NodeID: "forwarding-forbidden", Engine: "ollama", Settings: config})
	response, err := clientB.Post(server.URL+settingsPath+"apply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("paired mutation rejected: %d", response.StatusCode)
	}
	read(scanB, 2)
	read(scanC, 2)
	mu.Lock()
	seen := callerSeen
	mu.Unlock()
	if seen != "b" {
		t.Fatalf("caller not authenticated: %q", seen)
	}
	response, err = client(stranger).Post(server.URL+settingsPath+"apply", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("unpinned mutation accepted")
	}
	if err := os.Remove(filepath.Join(a, "trusted", "c.json")); err != nil {
		t.Fatal(err)
	}
	mesh.Refresh()
	ended := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resC.Body); ended <- err }()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("revoked peer stream did not close")
	}
	// A reconnected remaining peer atomically receives the latest full state.
	_, again := open(clientB)
	read(again, 2)
}

func TestSettingsRelayCorrelationCancellationAndCleanup(t *testing.T) {
	sent := make(chan settings.Relay, 2)
	canceled := make(chan string, 1)
	relay := &settingsRelay{send: func(method string, value any) error {
		data, _ := json.Marshal(value)
		if method == "engine:settings-request" {
			var p settings.Relay
			_ = json.Unmarshal(data, &p)
			sent <- p
		} else {
			var p struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(data, &p)
			canceled <- p.ID
		}
		return nil
	}}
	results := make(chan string, 2)
	for _, name := range []string{"one", "two"} {
		go func(name string) {
			result, err := relay.call(context.Background(), "preview", settings.Request{Engine: name}, "")
			if err != nil {
				results <- "error"
			} else {
				results <- string(result)
			}
		}(name)
	}
	first, second := <-sent, <-sent
	for _, request := range []settings.Relay{second, first} {
		data, _ := json.Marshal(settingsReply{ID: request.ID, Result: json.RawMessage(`"` + request.Request.Engine + `"`)})
		relay.reply(data)
	}
	got := map[string]bool{<-results: true, <-results: true}
	if !got[`"one"`] || !got[`"two"`] {
		t.Fatal("responses were not correlated")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := relay.call(ctx, "apply", settings.Request{}, ""); done <- err }()
	request := <-sent
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation ignored")
	}
	if <-canceled != request.ID {
		t.Fatal("wrong canceled correlation")
	}
	relay.mu.Lock()
	pending := len(relay.pending)
	relay.mu.Unlock()
	if pending != 0 {
		t.Fatal("relay leaked requests")
	}
}
