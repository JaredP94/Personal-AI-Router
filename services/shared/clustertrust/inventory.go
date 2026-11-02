// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// ReadPeerInventory accepts one bounded JSON response only when its TLS peer
// still matches the exact current pin and the authorization did not change
// while reading. Redirects are deliberately not followed, including HTTPS ones.
// An empty expected principal is used only to discover a manual peer; callers
// must then repeat with that exact principal before publishing inventory.
func ReadPeerInventory(ctx context.Context, mesh *Mesh, client *http.Client, principal, url, poll string, limit int64, value interface{}) (string, string, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil || mesh == nil {
		return "", "", false
	}
	mesh.Refresh()
	local := mesh.PeerRecordID(mesh.NodeUUID())
	binding := mesh.PeerRecordID(principal)
	if local == "" || (principal != "" && binding == "") {
		return "", "", false
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", false
	}
	if req.URL.Scheme != "https" {
		return "", "", false
	}
	if poll != "" {
		req.Header.Set("X-NVPAIR-Poll", poll)
	}
	resp, err := copyClient.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return "", "", false
	}
	cert := resp.TLS.PeerCertificates[0]
	if principal == "" {
		principal = UUIDFromCert(cert)
	}
	// Read the complete bounded body: a valid JSON prefix with excessive trailing
	// data is not an accepted response.
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit || json.Unmarshal(data, value) != nil {
		return "", "", false
	}
	mesh.Refresh()
	current := mesh.PeerRecordID(principal)
	if current == "" || local != mesh.PeerRecordID(mesh.NodeUUID()) || !mesh.VerifyPeerCertificate(principal, cert) || (binding != "" && binding != current) {
		return "", "", false
	}
	return principal, current, true
}
