// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Provider struct {
	cfg      Config
	tp       *sdktrace.TracerProvider
	tracer   trace.Tracer
	shutdown func(context.Context) error
}

func Init(ctx context.Context, cfg Config) (*Provider, error) {
	if !cfg.Enabled {
		return &Provider{cfg: cfg, tracer: otel.GetTracerProvider().Tracer(cfg.ServiceName)}, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(cfg.BatchTimeout)),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Provider{
		cfg:      cfg,
		tp:       tp,
		tracer:   tp.Tracer(cfg.ServiceName),
		shutdown: tp.Shutdown,
	}, nil
}

// NewTestProvider creates a Provider backed by the given TracerProvider.
// This is intended for unit tests using in-memory exporters.
func NewTestProvider(tp *sdktrace.TracerProvider, cfg Config) *Provider {
	otel.SetTracerProvider(tp)
	return &Provider{
		cfg:      cfg,
		tp:       tp,
		tracer:   tp.Tracer(cfg.ServiceName),
		shutdown: tp.Shutdown,
	}
}

func (p *Provider) Tracer() trace.Tracer {
	if p == nil || p.tracer == nil {
		return otel.GetTracerProvider().Tracer("nvpair")
	}
	return p.tracer
}

func (p *Provider) RecordPayloads() bool {
	return p != nil && p.cfg.RecordPayloads
}

func (p *Provider) Shutdown(ctx context.Context) error {
	if p != nil && p.shutdown != nil {
		return p.shutdown(ctx)
	}
	return nil
}

func init() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// InjectHTTPContext injects W3C traceparent into an outgoing HTTP request.
func InjectHTTPContext(ctx context.Context, req *http.Request) {
	if req == nil {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// ExtractHTTPContext extracts W3C traceparent from an incoming HTTP request.
func ExtractHTTPContext(req *http.Request) context.Context {
	if req == nil {
		return context.Background()
	}
	if req.Header == nil {
		return req.Context()
	}
	return otel.GetTextMapPropagator().Extract(req.Context(), propagation.HeaderCarrier(req.Header))
}

