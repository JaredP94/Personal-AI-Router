// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"nvpair-shared/enginenative"
)

// The engine's own admin endpoints. They live on the same port as the
// engine's inference API, and oMLX is the only engine that publishes them.
const (
	omlxStatsPath    = "/admin/api/stats"
	omlxActivityPath = "/admin/api/activity"
)

const (
	// defaultEngineNativeTimeout bounds one fetch of the engine's admin API,
	// so a slow engine cannot hold the em surface open. A fetch that runs out
	// of time is reported, never waited on.
	defaultEngineNativeTimeout = 5 * time.Second
	// engineNativeMaxBody caps how much of the engine's answer PAIR reads.
	engineNativeMaxBody = 1 << 20
)

// errNoEngineAdminAPI marks a node with no engine that publishes an admin
// API. It is a normal state, not a failure: the answer is "unsupported" with
// no error, so a consumer does not have to treat a missing engine as a fault.
var errNoEngineAdminAPI = errors.New("no engine publishes an admin API")

// The sanitised reasons a fetch can fail. They carry no part of the engine's
// answer and no part of the API key — the raw cause is logged, never
// returned, so nothing the engine said can travel to a peer through an error
// string.
const (
	engineNativeUnreachable = "the engine could not be reached"
	engineNativeTimedOut    = "the engine did not answer in time"
	engineNativeUnreadable  = "the engine's answer could not be read"
)

// engineAdminFailure is a failed fetch of the engine's admin API with a
// sanitised, user-facing message.
type engineAdminFailure struct {
	message string
	cause   error
}

func (f *engineAdminFailure) Error() string { return f.message }

func (f *engineAdminFailure) Unwrap() error { return f.cause }

// EngineNativeStats returns the local oMLX engine's own report of itself: the
// request and token counters, the throughput rates, the cache efficiency and
// the models the engine lists as active.
//
// The answer is PAIR's own type, mapped field by field from the engine's
// payload, so the engine's api_key — which its stats payload carries — has
// nowhere to go. A node whose engine publishes no admin API answers
// "unsupported" with no error; an engine that cannot be reached answers
// "unsupported" with a short, sanitised error.
func (e *Executor) EngineNativeStats(ctx context.Context) enginenative.Stats {
	raw, err := e.fetchEngineAdmin(ctx, omlxStatsPath)
	if err != nil {
		if errors.Is(err, errNoEngineAdminAPI) {
			return enginenative.Stats{Engine: enginenative.EngineOmlx, Supported: false}
		}
		slog.Debug("engine-native stats unavailable", "reason", err.Error(), "err", causeOf(err))
		return enginenative.Stats{Engine: enginenative.EngineOmlx, Supported: false, Error: err.Error()}
	}
	stats, err := enginenative.ParseStats(raw)
	if err != nil {
		slog.Debug("engine-native stats unreadable", "err", err)
		return enginenative.Stats{Engine: enginenative.EngineOmlx, Supported: false, Error: engineNativeUnreadable}
	}
	stats.Engine = enginenative.EngineOmlx
	stats.Supported = true
	// ObservedAt is when the snapshot was taken — the fetch, not the parse.
	stats.ObservedAt = time.Now().UnixMilli()
	return stats
}

// EngineNativeActivity returns the local oMLX engine's own report of the
// requests it is working on right now, one row per in-flight request, with
// the phase the engine reported it in.
//
// It follows EngineNativeStats: no admin API means "unsupported" with no
// error, an unreachable engine means "unsupported" with a short, sanitised
// error.
func (e *Executor) EngineNativeActivity(ctx context.Context) enginenative.Activity {
	raw, err := e.fetchEngineAdmin(ctx, omlxActivityPath)
	if err != nil {
		if errors.Is(err, errNoEngineAdminAPI) {
			return enginenative.Activity{Engine: enginenative.EngineOmlx, Supported: false}
		}
		slog.Debug("engine-native activity unavailable", "reason", err.Error(), "err", causeOf(err))
		return enginenative.Activity{Engine: enginenative.EngineOmlx, Supported: false, Error: err.Error()}
	}
	activity, err := enginenative.ParseActivity(raw)
	if err != nil {
		slog.Debug("engine-native activity unreadable", "err", err)
		return enginenative.Activity{Engine: enginenative.EngineOmlx, Supported: false, Error: engineNativeUnreadable}
	}
	activity.Engine = enginenative.EngineOmlx
	activity.Supported = true
	return activity
}

// fetchEngineAdmin reads one of the engine's admin endpoints on loopback and
// returns its body.
//
// It is bounded: a slow engine is waited on for at most the executor's
// engine-native timeout, and the answer is a failure rather than a hang. The
// engine's API key is sent as a bearer token, exactly as the engine's own
// actions send it, and it never appears in what this returns.
func (e *Executor) fetchEngineAdmin(ctx context.Context, path string) ([]byte, error) {
	port, ok := e.omlxAdminPort()
	if !ok {
		return nil, errNoEngineAdminAPI
	}
	timeout := e.engineNativeTimeout
	if timeout <= 0 {
		timeout = defaultEngineNativeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &engineAdminFailure{message: engineNativeUnreachable, cause: err}
	}
	if key := readOMLXAPIKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := e.client
	if client == nil {
		client = newEngineHTTPClient(engineResponseHeaderTimeout)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, adminFailure(ctx, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, engineNativeMaxBody))
	if err != nil {
		return nil, adminFailure(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &engineAdminFailure{message: fmt.Sprintf("the engine answered HTTP %d", resp.StatusCode)}
	}
	return body, nil
}

// adminFailure turns a transport failure into a sanitised one. A fetch that
// ran out of time says so, so a reader can tell a slow engine from a missing
// one; every other transport failure reads as "could not be reached".
func adminFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &engineAdminFailure{message: engineNativeTimedOut, cause: err}
	}
	return &engineAdminFailure{message: engineNativeUnreachable, cause: err}
}

// causeOf returns the underlying cause of a sanitised failure, so a log can
// carry it. Only the log does: what a caller receives is the sanitised
// message.
func causeOf(err error) error {
	var failure *engineAdminFailure
	if errors.As(err, &failure) {
		return failure.cause
	}
	return err
}

// omlxAdminPort returns the port of the local oMLX engine — the only engine
// that publishes an admin API.
//
// It reports false when this host knows no oMLX engine (it is not a known
// engine here, or it has no platform block for this host) and when the
// engine's port is not known yet. A host with no engine that publishes an
// admin API is a normal state, not a failure: the answer is "unsupported"
// with no error.
//
// A host that does know oMLX is asked, even when the engine is not running:
// the fetch fails, and the answer says so. That is what separates "there is
// no admin API here" from "the admin API did not answer".
func (e *Executor) omlxAdminPort() (int, bool) {
	st, err := e.state(enginenative.EngineOmlx)
	if err != nil {
		return 0, false
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	if port <= 0 {
		return 0, false
	}
	return port, true
}
