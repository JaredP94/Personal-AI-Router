// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func mustPinPath(t *testing.T, ts *TrustStore, uuid string) string {
	t.Helper()
	path, err := ts.pinPath(uuid)
	if err != nil {
		t.Fatalf("pin path: %v", err)
	}
	return path
}

// pinWithCommonName supplies a real certificate whose CN carries the principal,
// including crafted identifiers that a trusted peer can sign and endorse.
func pinWithCommonName(t *testing.T, uuid string) *TrustedPin {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: uuid},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return &TrustedPin{
		NodeUUID:  uuid,
		ClusterID: "cluster-1",
		CertPem:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
}

func TestTrustStoreRejectsUnsafePinIdentifiers(t *testing.T) {
	test := func(name, uuid string) {
		t.Run(name, func(t *testing.T) {
			ts, count := newAnnouncingStore(t)
			victim := filepath.Join(filepath.Dir(ts.dir), "victim.json")
			if err := os.WriteFile(victim, []byte("preserve me"), 0o600); err != nil {
				t.Fatalf("write victim: %v", err)
			}
			if err := ts.Pin(pinWithCommonName(t, uuid)); err == nil {
				t.Fatal("unsafe principal was pinned")
			}
			data, err := os.ReadFile(victim)
			if err != nil {
				t.Fatalf("read victim: %v", err)
			}
			if string(data) != "preserve me" {
				t.Errorf("victim file changed: %q", data)
			}
			entries, err := os.ReadDir(ts.dir)
			if err != nil {
				t.Fatalf("read trusted directory: %v", err)
			}
			if len(entries) != 0 || len(ts.List()) != 0 || count() != 0 {
				t.Errorf("rejected pin mutated store: files=%d pins=%d announcements=%d", len(entries), len(ts.List()), count())
			}
		})
	}
	test("empty principal", "")
	test("current directory", ".")
	test("parent directory", "..")
	test("Unix traversal", "../victim")
	test("Windows traversal", `..\victim`)
	test("nested Unix traversal", "sub/../../victim")
	test("nested Windows traversal", `sub\..\..\victim`)
	test("absolute path", filepath.Join(t.TempDir(), "victim"))
	test("drive relative path", "C:victim")
	test("UNC path", `\\server\share\victim`)
	test("alternate data stream", "peer:stream")
	test("NUL byte", "peer\x00victim")
	if runtime.GOOS == "windows" {
		test("Windows reserved device", "CON")
		test("Windows reserved serial port", "COM1")
		test("Windows reserved printer port", "LPT1")
	}
}

func TestTrustStoreRejectsUnsafeRemoveIdentifiers(t *testing.T) {
	test := func(name, uuid string) {
		t.Run(name, func(t *testing.T) {
			ts, count := newAnnouncingStore(t)
			pin := testPin(t, "safe-peer")
			if err := ts.Pin(pin); err != nil {
				t.Fatalf("pin safe peer: %v", err)
			}
			victim := filepath.Join(filepath.Dir(ts.dir), "victim.json")
			if err := os.WriteFile(victim, []byte("preserve me"), 0o600); err != nil {
				t.Fatalf("write victim: %v", err)
			}
			if err := ts.Remove(uuid); err == nil {
				t.Fatal("unsafe removal succeeded")
			}
			data, err := os.ReadFile(victim)
			if err != nil {
				t.Fatalf("read victim: %v", err)
			}
			if string(data) != "preserve me" {
				t.Errorf("victim file changed: %q", data)
			}
			if _, ok := ts.Get(pin.NodeUUID); !ok || len(ts.List()) != 1 || count() != 1 {
				t.Errorf("rejected removal mutated store: safePeer=%t pins=%d announcements=%d", ok, len(ts.List()), count())
			}
		})
	}
	test("empty principal", "")
	test("current directory", ".")
	test("parent directory", "..")
	test("Unix traversal", "../victim")
	test("Windows traversal", `..\victim`)
	test("nested Unix traversal", "sub/../../victim")
	test("nested Windows traversal", `sub\..\..\victim`)
	test("absolute path", filepath.Join(t.TempDir(), "victim"))
	test("drive relative path", "C:victim")
	test("UNC path", `\\server\share\victim`)
	test("alternate data stream", "peer:stream")
	test("NUL byte", "peer\x00victim")
	if runtime.GOOS == "windows" {
		test("Windows reserved device", "CON")
		test("Windows reserved serial port", "COM1")
		test("Windows reserved printer port", "LPT1")
	}
}

func TestTrustStoreLoadSkipsUnsafePinIdentifiers(t *testing.T) {
	test := func(name, uuid string) {
		t.Run(name, func(t *testing.T) {
			ts, err := newTrustStore(t.TempDir())
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			data, err := json.Marshal(pinWithCommonName(t, uuid))
			if err != nil {
				t.Fatalf("encode pin: %v", err)
			}
			if err := os.WriteFile(filepath.Join(ts.dir, uuid+".json"), data, 0o600); err != nil {
				t.Fatalf("write unsafe pin: %v", err)
			}
			reloaded, err := newTrustStore(filepath.Dir(ts.dir))
			if err != nil {
				t.Fatalf("reload store: %v", err)
			}
			if len(reloaded.List()) != 0 {
				t.Fatal("unsafe pin was loaded")
			}
		})
	}
	test("empty principal", "")
	test("current directory principal", ".")
	test("parent directory principal", "..")
}

func TestTrustStoreOpaqueSafeIdentifierSurvivesReload(t *testing.T) {
	ts, err := newTrustStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	pin := testPin(t, "peer.name-1")
	if err := ts.Pin(pin); err != nil {
		t.Fatalf("pin safe opaque identifier: %v", err)
	}
	reloaded, err := newTrustStore(filepath.Dir(ts.dir))
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	if got, ok := reloaded.Get(pin.NodeUUID); !ok || got.CertPem != pin.CertPem {
		t.Fatal("safe opaque identifier did not survive reload")
	}
	if err := reloaded.Remove(pin.NodeUUID); err != nil {
		t.Fatalf("remove safe pin: %v", err)
	}
	if err := reloaded.Remove(pin.NodeUUID); err != nil {
		t.Fatalf("repeat safe removal: %v", err)
	}
}

func TestMergeRosterRejectsTraversalPrincipal(t *testing.T) {
	m := newTestManager(t)
	peerUUID, peerCert, peerFP, peerKey := makeNode(t, "trusted-peer")
	pinTrusted(t, m, peerUUID, peerCert, peerFP)
	identityPath := filepath.Join(filepath.Dir(m.trust.dir), "identity.json")
	before, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read identity before merge: %v", err)
	}
	pin := pinWithCommonName(t, "../identity")
	fingerprint, err := certFingerprintFromPEM([]byte(pin.CertPem))
	if err != nil {
		t.Fatalf("fingerprint crafted certificate: %v", err)
	}
	endorsement := signEndorsement(peerKey, peerUUID, pin.NodeUUID, fingerprint, "cluster-1", time.Now().UnixMilli(), 1, 1)
	m.mergeRoster(&Roster{
		ClusterID: "cluster-1",
		Members: []RosterEntry{{
			NodeUUID: pin.NodeUUID, CertPem: pin.CertPem, CertFingerprint: fingerprint,
			AdmissionEpoch: 1, Endorsements: []Endorsement{endorsement},
		}},
	}, peerUUID)
	if _, ok := m.trust.Get(pin.NodeUUID); ok {
		t.Error("trusted endorsement admitted a traversal principal")
	}
	if _, ok := m.memberByNodeID(pin.NodeUUID); ok {
		t.Error("traversal principal became a member")
	}
	after, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read identity after merge: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("roster merge overwrote identity.json")
	}
}
