// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

var (
	multicastOnce sync.Once
	multicastErr  error
)

// requireMulticast skips a test that needs real mDNS discovery when this host
// cannot send IPv4 multicast at all. GitHub's hosted macOS runners are such a
// host: every send to 224.0.0.251 fails with "no route to host", so no node is
// ever discovered. On any machine that can multicast, the test runs as usual;
// the skip names the environment so it is never mistaken for a pass.
func requireMulticast(t *testing.T) {
	t.Helper()
	multicastOnce.Do(func() { multicastErr = probeMulticast() })
	if multicastErr != nil {
		t.Skipf("environment: this host cannot send mDNS multicast (%v); this test needs real mDNS discovery", multicastErr)
	}
}

// probeMulticast sends one mDNS PTR query for a name nothing answers, the same
// kind of packet discovery sends, and reports whether it left the host.
func probeMulticast() error {
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Write(mdnsProbeQuery())
	return err
}

func mdnsProbeQuery() []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[4:], 1) // one question
	for _, label := range []string{"_nvpair-ci-probe", "_tcp", "local"} {
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0, 0, 12, 0, 1) // root, type PTR, class IN
	return msg
}
