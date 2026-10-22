// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"nvpair-shared/cors"
)

const engineIdentityProbeHeader = "X-NVPAIR-Engine-Identity-Probe"

// nvpairRequestIDHeader carries PAIR's job id from the origin proxy to the
// peer that serves the request. It is the origin's own id for the request,
// and it is not a routing input: the ingress stays strictly terminal, so a
// peer's request is never re-routed onward.
const nvpairRequestIDHeader = "X-NVPAIR-Request-Id"

// nvpairOriginNodeHeader carries the ORIGIN node's id from the origin proxy
// to the peer that serves the request.
//
// It rides alongside the job id because PAIR's job id is a per-proxy counter:
// two origins can both hold a job "1", so the job id alone does not say
// which job a frame is about. The origin does.
const nvpairOriginNodeHeader = "X-NVPAIR-Origin-Node"

// nvpairRunIDHeader carries the ORIGIN proxy's run nonce from the origin
// proxy to the peer that serves the request.
//
// It rides alongside the job id because a proxy that restarts counts its jobs
// from 1 again: the run nonce says which run the job id belongs to, so a
// recycled job id is never taken for the job it replaced.
const nvpairRunIDHeader = "X-NVPAIR-Run-Id"

var plaintextRejectWarned sync.Map

func logPlaintextReject(r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if _, loaded := plaintextRejectWarned.LoadOrStore(host, struct{}{}); !loaded {
		slog.Warn("rejected non-loopback plaintext request; cluster peers must use mTLS",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
		return
	}
	slog.Debug("rejected non-loopback plaintext request; cluster peers must use mTLS",
		"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
}

// localBackend is the explicit loopback engine the cluster mTLS ingress
// forwards to. It is supplied by the broker over node/set-local-backend and is
// deliberately NOT sourced from the discovery overlay: a request that arrived
// over the LAN mTLS ingress can only ever be dumped on this node's own local
// engine, never re-routed to a peer, so the ingress path is strictly terminal
// and cannot recurse or amplify.
type localBackend struct {
	Engine  string `json:"engine"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Healthy bool   `json:"healthy"`
}

// setLocalBackend records (or, with a zero port / unhealthy flag, effectively
// clears) the local engine the ingress serves.
func (p *Proxy) setLocalBackend(b localBackend) {
	p.backendMu.Lock()
	p.backend = b
	p.backendMu.Unlock()
}

// localBackendTarget returns the loopback URL of the current local engine, and
// false when none is set/healthy (the ingress then answers 503 rather than
// forwarding). The host defaults to 127.0.0.1 and is always loopback.
func (p *Proxy) localBackendTarget() (*url.URL, bool) {
	p.backendMu.RLock()
	b := p.backend
	p.backendMu.RUnlock()
	if b.Port <= 0 || !b.Healthy {
		return nil, false
	}
	host := b.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(b.Port))}, true
}

// handlePlain is the plaintext personality: it accepts requests only from
// loopback and hands them to the full local router (handleHTTP). A non-loopback
// caller — any LAN peer — is refused; peers must use the mTLS ingress. This is
// what closes the former open-relay exposure (the listener still binds all
// interfaces for the TLS personality, but plaintext is loopback-only).
func (p *Proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRemote(r.RemoteAddr) {
		// Answer a non-loopback preflight ahead of the gate. It grants no access
		// on its own; the request that follows still receives the real 403. A
		// loopback preflight continues into handleHTTP so an available engine's
		// exact origin and credentials policy can be preserved.
		if cors.WritePreflight(w, r) {
			return
		}
		logPlaintextReject(r)
		writeIngressError(w, http.StatusForbidden, "loopback-only",
			"plaintext requests are accepted only from loopback; cluster peers must use the mTLS ingress")
		return
	}
	// Engine-manager marks identity/action requests so the federated model-list
	// facade can never satisfy LM Studio's own /v1/models readiness probe.
	if r.Header.Get(engineIdentityProbeHeader) == "1" {
		writeIngressError(w, http.StatusConflict, "proxy-facade", "the compatibility facade is not an LM Studio engine")
		return
	}
	p.handleHTTP(w, r)
}

// handleClusterIngress is the LAN mTLS personality: it authenticates the caller
// against this node's cluster pins and, once the peer is a trusted cluster
// member, forwards the request straight to the local loopback engine — exactly
// like the local plaintext path, with no route filtering. The mTLS pin is the
// sole authorization boundary (a trusted peer is treated like a local client),
// so the two personalities stay behaviorally identical toward the engine. It
// never calls resolveCandidates, so a peer request cannot be re-routed onward.
func (p *Proxy) handleClusterIngress(w http.ResponseWriter, r *http.Request) {
	// Re-derive membership and pins per request so a cluster left, or a peer
	// paired or removed, after startup is reflected immediately without a proxy
	// restart — a removed peer must stop being accepted right away, which is the
	// whole point of the gate.
	p.mesh.Refresh()
	peer, ok := p.mesh.VerifyClientPin(r)
	if !ok {
		writeIngressError(w, http.StatusForbidden, "cluster-auth",
			"client certificate is not a pinned member of this node's cluster")
		return
	}
	target, ok := p.localBackendTarget()
	if !ok {
		writeIngressError(w, http.StatusServiceUnavailable, "no-local-backend",
			"no local inference backend is available on this node")
		return
	}
	// The job's identity, when the request came from a peer: the origin
	// proxy's job id, the origin's node id and the origin's run nonce all
	// travelled with the request. The frame the serving side publishes is
	// attributed to that origin, not to this node.
	job := peerJob{
		jobID:        r.Header.Get(nvpairRequestIDHeader),
		originNodeID: r.Header.Get(nvpairOriginNodeHeader),
		runID:        r.Header.Get(nvpairRunIDHeader),
		start:        time.Now(),
	}
	slog.Debug("cluster ingress forwarding to local backend",
		"peer", peer, "method", r.Method, "path", r.URL.Path, "target", target.Host,
		"job_id", job.jobID, "origin_node_id", job.originNodeID, "run_id", job.runID)
	p.reverseProxyToLocal(w, r, target, job)
}

// reverseProxyToLocal streams the request to the local engine, preserving
// cancellation (the request context is the proxy's root context, so a client
// disconnect or shutdown tears down the upstream call and stops generation).
//
// A request a peer forwarded carries the origin's job identity, and the
// serving side publishes the live frame under it: the frame is attributed to
// the origin, so every member of the cluster keys the job's live view the
// same way and the job's terminal event retires it.
func (p *Proxy) reverseProxyToLocal(w http.ResponseWriter, r *http.Request, target *url.URL, job peerJob) {
	p.newLocalReverseProxy(target, job).ServeHTTP(w, r)
}

// newLocalReverseProxy builds the ingress's reverse proxy to the local
// engine.
//
// A peer-served request is measured on the way back: the reader counts the
// bytes the engine sent, and the frame leaves attributed to the origin. A
// request this node received directly is served by its own router, which
// publishes the frame itself.
func (p *Proxy) newLocalReverseProxy(target *url.URL, job peerJob) *httputil.ReverseProxy {
	// The peer-served job's own limiter: the frame the serving side
	// leaves for it leaves at most once per window.
	limiter := newProgressLimiter()
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			stripTraceContext(req.Header)
		},
		Transport:  p.plainHTTPTransport(),
		BufferPool: p.bufPool,
		ModifyResponse: func(resp *http.Response) error {
			if job.jobID == "" {
				return nil
			}
			reader := newPeerProgressReader(resp.Body, job.start)
			reader.onProgress = func(prog peerProgress) {
				p.publishPeerProgress(job, limiter, prog)
			}
			resp.Body = reader
			return nil
		},
		ErrorHandler: func(ew http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("cluster ingress upstream error", "target", target.Host, "err", err)
			writeIngressError(ew, http.StatusBadGateway, "backend-error", "local inference backend error")
		},
	}
}

// isLoopbackRemote reports whether an http.Request RemoteAddr (host:port) is a
// loopback address (127.0.0.0/8 or ::1). An unparseable/empty RemoteAddr is not
// loopback, so it fails closed.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeIngressError writes a small structured JSON error. It never echoes the
// request body or any generated output. CORS headers are included because these
// are the proxy's own rejections: without them a browser client cannot read the
// status or reason, and every one of them looks like a generic CORS failure.
func writeIngressError(w http.ResponseWriter, status int, code, msg string) {
	cors.Apply(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	body, err := json.Marshal(map[string]string{"error": msg, "code": code})
	if err != nil {
		body = []byte(`{"error":"ingress error"}`)
	}
	_, _ = w.Write(body)
}
