// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

var errFirstBodyTimeout = errors.New("upstream timed out before first response body byte")

// A cooldown is evidence of a full silent deadline, not a liveness verdict.
// State is local to this proxy; success or time alone restores normal ordering.
func (p *Proxy) noteStalled(id string, now time.Time) {
	p.coolMu.Lock()
	defer p.coolMu.Unlock()
	if p.coolUntil == nil {
		p.coolUntil = make(map[string]time.Time)
	}
	p.coolUntil[id] = now.Add(proxyResponseTimeout)
}

func (p *Proxy) noteSuccess(id string) {
	p.coolMu.Lock()
	defer p.coolMu.Unlock()
	delete(p.coolUntil, id)
}

func (p *Proxy) coolingSet(now time.Time) map[string]bool {
	p.coolMu.Lock()
	defer p.coolMu.Unlock()
	if len(p.coolUntil) == 0 {
		return nil
	}
	cooling := make(map[string]bool, len(p.coolUntil))
	for id, until := range p.coolUntil {
		if now.Before(until) {
			cooling[id] = true
		} else {
			delete(p.coolUntil, id)
		}
	}
	return cooling
}

// Stable partition: keep every candidate, including a sole cooling candidate.
func demoteCooling(candidates []candidate, cooling map[string]bool) []candidate {
	if len(cooling) == 0 || len(candidates) < 2 {
		return candidates
	}
	out := make([]candidate, 0, len(candidates))
	for _, cand := range candidates {
		if !cooling[cand.id] {
			out = append(out, cand)
		}
	}
	for _, cand := range candidates {
		if cooling[cand.id] {
			out = append(out, cand)
		}
	}
	return out
}

// Observe the response-header wait after the request was written, not dial or
// TLS time. A shorter caller deadline, cancellation, or fast transport failure
// is not evidence that this candidate spent a full response deadline silent.
type stallTransport struct {
	base    http.RoundTripper
	proxy   *Proxy
	id      string
	timeout time.Duration
}

func (t stallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var mu sync.Mutex
	var written time.Time
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			mu.Lock()
			written = time.Now()
			mu.Unlock()
		}
	}}
	resp, err := t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	mu.Lock()
	waitStart := written
	mu.Unlock()
	var timeout net.Error
	if err != nil && req.Context().Err() == nil && errors.As(err, &timeout) &&
		timeout.Timeout() && !waitStart.IsZero() && time.Since(waitStart) >= t.timeout {
		t.proxy.noteStalled(t.id, time.Now())
	}
	return resp, err
}

type bodyWithPeek struct {
	io.Reader
	io.Closer
}

// Hold inference headers until content (or a valid empty EOF) arrives. On an
// error the caller returns from ModifyResponse, so ReverseProxy closes the
// original body and unblocks the pending read. The buffered channel lets that
// reader exit even if the deadline or caller cancellation won.
func awaitFirstBody(ctx context.Context, body io.ReadCloser, budget time.Duration) (io.ReadCloser, error) {
	type result struct {
		data []byte
		err  error
	}
	ready := make(chan result, 1)
	go func() {
		// Match ReverseProxy's copy buffer. Reading only one byte would split
		// the first engine frame and publish progress before it can be parsed.
		first := make([]byte, 32*1024)
		n, err := body.Read(first)
		ready <- result{data: first[:n], err: err}
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case got := <-ready:
		if len(got.data) == 0 && got.err != nil && !errors.Is(got.err, io.EOF) {
			return nil, got.err
		}
		return bodyWithPeek{Reader: io.MultiReader(bytes.NewReader(got.data), body), Closer: body}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errFirstBodyTimeout
	}
}
