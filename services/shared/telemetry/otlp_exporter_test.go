// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
)

type receivedExport struct {
	request   *collectortrace.ExportTraceServiceRequest
	headers   metadata.MD
	remaining time.Duration
}

type plaintextTraceCollector struct {
	collectortrace.UnimplementedTraceServiceServer
	requests    chan receivedExport
	compression chan string
}

func (c *plaintextTraceCollector) Export(ctx context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	headers, _ := metadata.FromIncomingContext(ctx)
	deadline, _ := ctx.Deadline()
	c.requests <- receivedExport{request: req, headers: headers.Copy(), remaining: time.Until(deadline)}
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

func (c *plaintextTraceCollector) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (c *plaintextTraceCollector) HandleRPC(_ context.Context, event stats.RPCStats) {
	switch event := event.(type) {
	case *stats.InHeader:
		c.compression <- event.Compression
	}
}

func (c *plaintextTraceCollector) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (c *plaintextTraceCollector) HandleConn(context.Context, stats.ConnStats) {}

// These credentials are generated only in a temporary test directory. Valid
// certificate files are essential: missing/invalid files do not activate the
// upstream environment-derived TLS credentials that caused the regression.
func temporaryOTLPCredentials(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-only"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestOTLPExporterIgnoresEnvironment(t *testing.T) {
	certPath, keyPath := temporaryOTLPCredentials(t)
	for _, prefix := range []string{"OTEL_EXPORTER_OTLP_", "OTEL_EXPORTER_OTLP_TRACES_"} {
		for _, credentials := range []string{"root-certificate", "client-certificate"} {
			t.Run(prefix+credentials, func(t *testing.T) {
				// Isolate each general and trace-specific credential source,
				// including when the test runner itself inherited OTLP settings.
				for _, p := range []string{"OTEL_EXPORTER_OTLP_", "OTEL_EXPORTER_OTLP_TRACES_"} {
					for _, suffix := range []string{"CERTIFICATE", "CLIENT_CERTIFICATE", "CLIENT_KEY", "ENDPOINT", "HEADERS", "COMPRESSION", "TIMEOUT", "INSECURE", "PROTOCOL"} {
						t.Setenv(p+suffix, "")
					}
				}
				if credentials == "root-certificate" {
					t.Setenv(prefix+"CERTIFICATE", certPath)
				} else {
					t.Setenv(prefix+"CLIENT_CERTIFICATE", certPath)
					t.Setenv(prefix+"CLIENT_KEY", keyPath)
				}
				t.Setenv(prefix+"ENDPOINT", "https://127.0.0.2:1/ignored")
				t.Setenv(prefix+"HEADERS", "x-env-header=must-not-export,authorization=must-not-export")
				t.Setenv(prefix+"COMPRESSION", "none")
				t.Setenv(prefix+"TIMEOUT", "60000")
				t.Setenv(prefix+"INSECURE", "false")
				t.Setenv(prefix+"PROTOCOL", "http/protobuf")

				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				collector := &plaintextTraceCollector{requests: make(chan receivedExport, 1), compression: make(chan string, 1)}
				server := grpc.NewServer(grpc.StatsHandler(collector)) // Intentionally no TLS.
				collectortrace.RegisterTraceServiceServer(server, collector)
				served := make(chan error, 1)
				go func() { served <- server.Serve(listener) }()
				t.Cleanup(func() {
					server.Stop()
					if err := <-served; err != nil {
						t.Errorf("serve: %v", err)
					}
				})

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				endpoint := listener.Addr().String()
				exporter, err := OTLPExporter(ctx, endpoint)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if err := exporter.Shutdown(ctx); err != nil {
						t.Errorf("exporter shutdown: %v", err)
					}
				})
				span := tracetest.SpanStub{Name: "loopback-export"}.Snapshot()
				if err := exporter.ExportSpans(ctx, []sdktrace.ReadOnlySpan{span}); err != nil {
					t.Fatalf("plaintext loopback export with inherited OTLP settings: %v", err)
				}

				select {
				case got := <-collector.requests:
					resources := got.request.GetResourceSpans()
					if len(resources) != 1 || len(resources[0].GetScopeSpans()) != 1 || len(resources[0].GetScopeSpans()[0].GetSpans()) != 1 || resources[0].GetScopeSpans()[0].GetSpans()[0].GetName() != "loopback-export" {
						t.Fatal("collector did not receive the exported span")
					}
					for _, key := range []string{"x-env-header", "authorization"} {
						if len(got.headers.Get(key)) != 0 {
							t.Errorf("inherited %s header reached the collector", key)
						}
					}
					if authority := got.headers.Get(":authority"); len(authority) != 1 || authority[0] != endpoint {
						t.Errorf("authority = %v; want %s", authority, endpoint)
					}
					if got.remaining <= 0 || got.remaining > 2*time.Second {
						t.Errorf("export deadline remaining = %v; want at most 2s", got.remaining)
					}
				case <-ctx.Done():
					t.Fatal("collector did not receive a request")
				}
				select {
				case got := <-collector.compression:
					if got != "gzip" {
						t.Errorf("compression = %q; want pinned gzip", got)
					}
				case <-ctx.Done():
					t.Fatal("collector did not receive RPC headers")
				}
			})
		}
	}
}
