// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type recordingExporter struct {
	mu      sync.Mutex
	spans   []sdktrace.ReadOnlySpan
	err     error
	stopped chan struct{}
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return e.err
}

func (e *recordingExporter) Shutdown(context.Context) error {
	close(e.stopped)
	return nil
}

func (e *recordingExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.spans)
}

func newTestManager(t *testing.T) (*Manager, *recordingExporter) {
	t.Helper()
	e := &recordingExporter{stopped: make(chan struct{})}
	m := NewManager("test-service", func(_ context.Context, endpoint string) (sdktrace.SpanExporter, error) {
		if endpoint != "127.0.0.1:4317" {
			t.Errorf("endpoint = %q", endpoint)
		}
		return e, nil
	})
	t.Cleanup(func() {
		if err := m.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return m, e
}

func applySettings(t *testing.T, m *Manager, rev uint64, s Settings) {
	t.Helper()
	if got, err := m.Apply(context.Background(), rev, s); err != nil || got != rev {
		t.Fatalf("Apply = %d, %v; want %d, nil", got, err, rev)
	}
}

func TestDisabledSessionUsesNoopTracer(t *testing.T) {
	m := NewManager("disabled", func(context.Context, string) (sdktrace.SpanExporter, error) {
		t.Fatal("disabled manager created an exporter")
		return nil, nil
	})
	defer m.Shutdown(context.Background())
	s := m.Session()
	_, span := s.Tracer().Start(context.Background(), "disabled")
	defer span.End()
	if span.IsRecording() || s.RecordPayloads() || s.Revision() != 0 || !s.Active() {
		t.Fatal("unexpected default session state")
	}
	if got := m.Status(); got.Revision != 0 || got.Enabled || !got.LastExportOK.IsZero() || got.LastExportError != "" {
		t.Fatalf("default status = %+v", got)
	}
}

func TestApplyIgnoresStaleRevision(t *testing.T) {
	m, _ := newTestManager(t)
	applySettings(t, m, 3, Settings{Enabled: true, CollectorPort: 4317})
	old := m.Session()
	for _, rev := range []uint64{2, 3} {
		if got, err := m.Apply(context.Background(), rev, Settings{CollectorPort: 4317}); err != nil || got != 3 {
			t.Fatalf("Apply(%d) = %d, %v", rev, got, err)
		}
	}
	if m.Status().Revision != 3 || !m.Status().Enabled || m.Session() != old || !old.Active() {
		t.Fatal("stale/equal revision changed session")
	}
}

func TestRevokedRevisionExportsNothing(t *testing.T) {
	m, e := newTestManager(t)
	applySettings(t, m, 1, Settings{Enabled: true, CollectorPort: 4317})
	old := m.Session()
	_, span := old.Tracer().Start(context.Background(), "queued-before-revocation")
	span.End()
	applySettings(t, m, 2, Settings{CollectorPort: 4317})
	if err := old.tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("superseded provider was not shut down")
	}
	if got := e.count(); got != 0 {
		t.Fatalf("exported %d revoked spans", got)
	}
}

func TestCaptureRevokedSessionReportsNoPayloads(t *testing.T) {
	m, _ := newTestManager(t)
	applySettings(t, m, 1, Settings{Enabled: true, CollectorPort: 4317, RecordPayloads: true})
	old := m.Session()
	if !old.RecordPayloads() || !old.Active() || old.Revision() != 1 {
		t.Fatal("new payload session is not active")
	}
	applySettings(t, m, 2, Settings{CollectorPort: 4317})
	if old.RecordPayloads() || old.Active() || old.Revision() != 1 {
		t.Fatal("superseded session retained consent or changed revision")
	}
	_, span := m.Session().Tracer().Start(context.Background(), "disabled")
	defer span.End()
	if span.IsRecording() {
		t.Fatal("disabled revision records spans")
	}
}

func TestNoGlobalProviderMutation(t *testing.T) {
	provider, propagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	m, _ := newTestManager(t)
	applySettings(t, m, 1, Settings{Enabled: true, CollectorPort: 4317})
	if otel.GetTracerProvider() != provider || !reflect.DeepEqual(otel.GetTextMapPropagator(), propagator) {
		t.Fatal("manager mutated OpenTelemetry globals")
	}
}

func TestApplyInvalidSettingsPreservesSession(t *testing.T) {
	m, _ := newTestManager(t)
	applySettings(t, m, 1, Settings{Enabled: true, CollectorPort: 4317})
	old := m.Session()
	if rev, err := m.Apply(context.Background(), 2, Settings{Enabled: true}); err == nil || rev != 1 {
		t.Fatalf("invalid Apply = %d, %v", rev, err)
	}
	if m.Session() != old || !old.Active() {
		t.Fatal("invalid settings revoked current session")
	}
}

func TestExporterCreationFailurePreservesSession(t *testing.T) {
	want := errors.New("exporter unavailable")
	m := NewManager("test", func(context.Context, string) (sdktrace.SpanExporter, error) { return nil, want })
	defer m.Shutdown(context.Background())
	old := m.Session()
	if rev, err := m.Apply(context.Background(), 1, Settings{Enabled: true, CollectorPort: 4317}); !errors.Is(err, want) || rev != 0 {
		t.Fatalf("Apply = %d, %v", rev, err)
	}
	if m.Session() != old || !old.Active() {
		t.Fatal("factory failure replaced current session")
	}
}

func TestExportStatusAndServiceResource(t *testing.T) {
	m, e := newTestManager(t)
	applySettings(t, m, 1, Settings{Enabled: true, CollectorPort: 4317})
	s := m.Session()
	flush := func() error {
		_, span := s.Tracer().Start(context.Background(), "export")
		span.End()
		return s.tp.ForceFlush(context.Background())
	}
	if err := flush(); err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if status.LastExportOK.IsZero() || status.LastExportError != "" || !status.LastExportErrorAt.IsZero() || e.count() != 1 {
		t.Fatalf("successful export status = %+v", status)
	}
	e.mu.Lock()
	attrs := e.spans[0].Resource().Attributes()
	e.err = errors.New("collector unavailable")
	e.mu.Unlock()
	found := false
	for _, a := range attrs {
		if a.Key == "service.name" && a.Value.AsString() == "test-service" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing service.name resource")
	}
	if err := flush(); err == nil {
		t.Fatal("failed export returned no error")
	}
	status = m.Status()
	if status.LastExportError != "collector unavailable" || status.LastExportErrorAt.IsZero() {
		t.Fatalf("failed export status = %+v", status)
	}
	e.mu.Lock()
	e.err = nil
	e.mu.Unlock()
	if err := flush(); err != nil {
		t.Fatal(err)
	}
	if status = m.Status(); status.LastExportOK.IsZero() || status.LastExportError != "" {
		t.Fatalf("recovered export status = %+v", status)
	}
}

func TestShutdownRevokesAndDropsQueuedSpans(t *testing.T) {
	m, e := newTestManager(t)
	applySettings(t, m, 1, Settings{Enabled: true, CollectorPort: 4317, RecordPayloads: true})
	s := m.Session()
	_, span := s.Tracer().Start(context.Background(), "queued")
	span.End()
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Active() || s.RecordPayloads() || m.Status().Enabled || e.count() != 0 || m.Session() == nil {
		t.Fatal("shutdown retained consent or exported queued spans")
	}
	if _, err := m.Apply(context.Background(), 2, Settings{Enabled: true, CollectorPort: 4317}); err == nil {
		t.Fatal("Apply restarted a shut down manager")
	}
}

type blockingExporter struct {
	started  chan struct{}
	canceled chan struct{}
	stopped  chan time.Duration
}

func (e *blockingExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	close(e.started)
	<-ctx.Done()
	close(e.canceled)
	return ctx.Err()
}

func (e *blockingExporter) Shutdown(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		e.stopped <- 0
	} else {
		e.stopped <- time.Until(deadline)
	}
	return nil
}

func TestSupersedingCancelsInflightExport(t *testing.T) {
	e := &blockingExporter{started: make(chan struct{}), canceled: make(chan struct{}), stopped: make(chan time.Duration, 1)}
	m := NewManager("test", func(context.Context, string) (sdktrace.SpanExporter, error) { return e, nil })
	defer m.Shutdown(context.Background())
	applySettings(t, m, 1, Settings{Enabled: true, CollectorPort: 4317})
	s := m.Session()
	_, span := s.Tracer().Start(context.Background(), "inflight")
	span.End()
	flushed := make(chan error, 1)
	go func() { flushed <- s.tp.ForceFlush(context.Background()) }()
	select {
	case <-e.started:
	case <-time.After(3 * time.Second):
		t.Fatal("export never started")
	}
	applySettings(t, m, 2, Settings{CollectorPort: 4317})
	select {
	case <-e.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("inflight export not canceled")
	}
	select {
	case remaining := <-e.stopped:
		if remaining <= 0 || remaining > 2*time.Second {
			t.Fatalf("shutdown deadline = %v", remaining)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exporter not shut down")
	}
	<-flushed
	if status := m.Status(); !status.LastExportOK.IsZero() || status.LastExportError != "" {
		t.Fatalf("revoked export changed status: %+v", status)
	}
}

func TestManagerConcurrentAccess(t *testing.T) {
	m := NewManager("test", func(context.Context, string) (sdktrace.SpanExporter, error) {
		return &recordingExporter{stopped: make(chan struct{})}, nil
	})
	defer m.Shutdown(context.Background())
	var wg sync.WaitGroup
	for i := uint64(1); i <= 20; i++ {
		wg.Add(1)
		go func(rev uint64) {
			defer wg.Done()
			_, err := m.Apply(context.Background(), rev, Settings{Enabled: true, CollectorPort: 4317, RecordPayloads: true})
			if err != nil {
				t.Error(err)
			}
			s := m.Session()
			s.RecordPayloads()
			s.Active()
			m.Status()
			_, span := s.Tracer().Start(context.Background(), "concurrent")
			span.End()
		}(i)
	}
	wg.Wait()
	if m.Status().Revision != 20 {
		t.Fatal("lost newest revision")
	}
}

func TestOTLPExporterRejectsNonLoopbackEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "example.com:4317", "127.0.0.2:4317", "localhost:4317", "127.0.0.1:0", "127.0.0.1:70000", "http://127.0.0.1:4317"} {
		if e, err := OTLPExporter(context.Background(), endpoint); err == nil {
			e.Shutdown(context.Background())
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
}

func TestExtractHTTPContext(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("baggage", "secret=not-imported")
	sc := trace.SpanContextFromContext(ExtractHTTPContext(req))
	if !sc.IsRemote() || sc.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("unexpected extracted span context: %v", sc)
	}
	if baggage.FromContext(ExtractHTTPContext(req)).Len() != 0 {
		t.Fatal("imported baggage")
	}
	if ExtractHTTPContext(nil) == nil {
		t.Fatal("nil context")
	}
	req.Header = nil
	if ExtractHTTPContext(req) != req.Context() {
		t.Fatal("lost request context")
	}
}
