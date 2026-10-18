// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const shutdownTimeout = 2 * time.Second

type ExporterFactory func(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error)

// OTLPExporter uses plaintext gRPC only to the configured IPv4 loopback port.
// Explicit transport options and a fixed dialer prevent environment-configured
// collector endpoints, TLS credentials or HTTP proxies from redirecting traffic.
func OTLPExporter(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host != "127.0.0.1" {
		return nil, errors.New("collector endpoint must be 127.0.0.1:<port>")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("collector port must be between 1 and 65535")
	}
	return otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		// Pin the exporter's credential field: environment-derived TLS credentials
		// otherwise take precedence over WithInsecure and gRPC dial options.
		otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()),
		otlptracegrpc.WithHeaders(nil),
		otlptracegrpc.WithTimeout(shutdownTimeout),
		// v1.34 only exposes gzip as a valid explicit compressor. Pin it rather
		// than inherit compression or log an error for an unsupported "none".
		otlptracegrpc.WithCompressor("gzip"),
		otlptracegrpc.WithDialOption(
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", endpoint)
			}),
		),
	)
}

type Status struct {
	Revision          uint64
	Enabled           bool
	LastExportOK      time.Time
	LastExportError   string
	LastExportErrorAt time.Time
}

// Manager serializes policy revisions and owns every session's lifetime.
// It never installs providers or propagators in OpenTelemetry's global state.
type Manager struct {
	mu           sync.Mutex
	serviceName  string
	newExporter  ExporterFactory
	session      *Session
	status       Status
	closed       bool
	retiring     sync.WaitGroup
	shutdownDone chan struct{}
	shutdownErr  error
}

// Session's revision, tracer and policy are immutable. Active and RecordPayloads
// additionally consult the manager so old requests cannot retain payload consent.
type Session struct {
	manager  *Manager
	revision uint64
	payloads bool
	tracer   trace.Tracer
	tp       *sdktrace.TracerProvider
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewManager(serviceName string, newExporter ExporterFactory) *Manager {
	m := &Manager{serviceName: serviceName, newExporter: newExporter, shutdownDone: make(chan struct{})}
	m.session = m.newSession(0, false)
	return m
}

func (m *Manager) newSession(revision uint64, payloads bool) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		manager: m, revision: revision, payloads: payloads,
		tracer: noop.NewTracerProvider().Tracer(m.serviceName), ctx: ctx, cancel: cancel,
	}
}

func (m *Manager) Apply(ctx context.Context, revision uint64, s Settings) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.status.Revision
	if m.closed {
		return current, errors.New("telemetry manager is shut down")
	}
	if err := ValidateSettings(s); err != nil {
		return current, err
	}
	if revision <= current {
		return current, nil
	}

	next := m.newSession(revision, s.RecordPayloads)
	if s.Enabled {
		exporter, err := m.newExporter(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(s.CollectorPort)))
		if err != nil {
			next.cancel()
			return current, fmt.Errorf("create otlp exporter: %w", err)
		}
		gate := &gatedExporter{session: next, exporter: exporter}
		next.tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(gate, sdktrace.WithExportTimeout(shutdownTimeout)),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
			sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceName(m.serviceName))),
		)
		next.tracer = next.tp.Tracer(m.serviceName)
	}

	old := m.session
	old.cancel()
	m.session = next
	m.status = Status{Revision: revision, Enabled: s.Enabled}
	m.retire(old)
	return revision, nil
}

// retire must be called with mu held, before Shutdown starts waiting.
func (m *Manager) retire(s *Session) {
	if s.tp == nil {
		return
	}
	m.retiring.Add(1)
	go func() {
		defer m.retiring.Done()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		// The gate is already closed; shutdown flushes cannot export queued spans.
		_ = s.tp.Shutdown(ctx)
	}()
}

func (m *Manager) Session() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.status.Enabled = false
		s := m.session
		s.cancel()
		go func() {
			if s.tp != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
				m.shutdownErr = s.tp.Shutdown(shutdownCtx)
				cancel()
			}
			m.retiring.Wait()
			close(m.shutdownDone)
		}()
	}
	m.mu.Unlock()
	select {
	case <-m.shutdownDone:
		return m.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) Tracer() trace.Tracer { return s.tracer }
func (s *Session) Revision() uint64     { return s.revision }
func (s *Session) RecordPayloads() bool { return s.payloads && s.Active() }

func (s *Session) Active() bool {
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && m.session == s
}

// gatedExporter admits exports under the same lock that revokes sessions.
// Already-admitted RPCs are canceled on revocation; spans delivered before the
// switch cannot be recalled. Queued exports and shutdown flushes are dropped.
type gatedExporter struct {
	session  *Session
	exporter sdktrace.SpanExporter
}

func (g *gatedExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	s := g.session
	m := s.manager
	m.mu.Lock()
	if m.closed || m.session != s {
		m.mu.Unlock()
		return nil
	}
	exportCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	m.mu.Unlock()
	defer cancel()
	defer stop()

	err := g.exporter.ExportSpans(exportCtx, spans)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.session != s {
		return nil
	}
	if err != nil {
		m.status.LastExportError = err.Error()
		m.status.LastExportErrorAt = time.Now()
	} else {
		m.status.LastExportOK = time.Now()
		m.status.LastExportError = ""
	}
	return err
}

func (g *gatedExporter) Shutdown(ctx context.Context) error { return g.exporter.Shutdown(ctx) }

var clientTraceContext = propagation.TraceContext{}

// ExtractHTTPContext accepts W3C context from an incoming local client request.
// It does not import baggage, use a global propagator, or inject outbound context.
func ExtractHTTPContext(req *http.Request) context.Context {
	if req == nil {
		return context.Background()
	}
	if req.Header == nil {
		return req.Context()
	}
	return clientTraceContext.Extract(req.Context(), propagation.HeaderCarrier(req.Header))
}
