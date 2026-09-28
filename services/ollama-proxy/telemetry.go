// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"nvpair-shared/telemetry"
)

func defaultTelemetryConfig() telemetry.Config {
	cfg := telemetry.DefaultConfig("nvpair-ollama-proxy")
	if ep := os.Getenv("PAIR_TELEMETRY_ENDPOINT"); ep != "" {
		cfg.Endpoint = ep
	} else if ep := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); ep != "" {
		cfg.Endpoint = ep
	}
	if v := os.Getenv("PAIR_TELEMETRY_ENABLED"); v != "" {
		cfg.Enabled = v == "true" || v == "1"
	} else if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		cfg.Enabled = false
	}
	if v := os.Getenv("PAIR_TELEMETRY_RECORD_PAYLOADS"); v != "" {
		cfg.RecordPayloads = v == "true" || v == "1"
	}
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		cfg.ServiceName = name
	}
	return cfg
}

func isStreamingRequest(path string, bodyBytes []byte) bool {
	if len(bodyBytes) > 0 {
		var payload struct {
			Stream *bool `json:"stream"`
		}
		if err := json.Unmarshal(bodyBytes, &payload); err == nil && payload.Stream != nil {
			return *payload.Stream
		}
	}
	if path == "/api/generate" || path == "/api/chat" {
		return true
	}
	return false
}

func extractPrompt(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	if p, ok := raw["prompt"]; ok {
		var str string
		if err := json.Unmarshal(p, &str); err == nil {
			return str
		}
		return string(p)
	}
	if m, ok := raw["messages"]; ok {
		var str string
		if err := json.Unmarshal(m, &str); err == nil {
			return str
		}
		return string(m)
	}
	return ""
}

func isLocalHost(hostPort string) bool {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func recordRequestHeaders(span trace.Span, header http.Header) {
	if span == nil || !span.IsRecording() || len(header) == 0 {
		return
	}
	redacted := telemetry.RedactHeaders(header)
	for k, v := range redacted {
		span.SetAttributes(attribute.String("http.request.header."+strings.ToLower(k), v))
	}
}

func (p *Proxy) tracer() trace.Tracer {
	if p != nil && p.telemetry != nil {
		return p.telemetry.Tracer()
	}
	return otel.GetTracerProvider().Tracer("nvpair-ollama-proxy")
}

type telemetryBodyReader struct {
	io.ReadCloser
	span           trace.Span
	start          time.Time
	recordPayloads bool
	firstReadOnce  sync.Once
	closeOnce      sync.Once
	lineBuf        []byte
	completionBuf  strings.Builder
	promptTokens   int
	outputTokens   int
	responseModel  string
}

func newTelemetryBodyReader(body io.ReadCloser, span trace.Span, start time.Time, recordPayloads bool) *telemetryBodyReader {
	return &telemetryBodyReader{
		ReadCloser:     body,
		span:           span,
		start:          start,
		recordPayloads: recordPayloads,
	}
}

func (r *telemetryBodyReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if n > 0 {
		r.recordFirstToken()
		r.observe(b[:n])
	}
	if err == io.EOF {
		if len(r.lineBuf) > 0 {
			r.inspectLine(r.lineBuf)
			r.lineBuf = nil
		}
		r.finalize()
	}
	return n, err
}

func (r *telemetryBodyReader) recordFirstToken() {
	r.firstReadOnce.Do(func() {
		if r.span != nil && r.span.IsRecording() {
			ttftMs := time.Since(r.start).Milliseconds()
			r.span.SetAttributes(attribute.Int64("pair.inference.ttft_ms", ttftMs))
			r.span.AddEvent("first_token")
		}
	})
}

const (
	maxTelemetryLineBytes        = 4 << 20 // 4 MiB max line buffer for large non-streaming frames
	maxRecordedCompletionBytes   = 64 << 10 // 64 KiB cap for recorded completion payload
)

func (r *telemetryBodyReader) observe(chunk []byte) {
	r.lineBuf = append(r.lineBuf, chunk...)
	for {
		end := bytes.IndexByte(r.lineBuf, '\n')
		if end < 0 {
			if len(r.lineBuf) > maxTelemetryLineBytes {
				r.lineBuf = nil
			}
			return
		}
		r.inspectLine(r.lineBuf[:end])
		r.lineBuf = r.lineBuf[end+1:]
	}
}

type streamChunk struct {
	Model    string `json:"model"`
	Response string `json:"response"`
	Message  *struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Choices []struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Done            bool `json:"done"`
	PromptEvalCount int  `json:"prompt_eval_count"`
	EvalCount       int  `json:"eval_count"`
	Usage           *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (r *telemetryBodyReader) inspectLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if bytes.HasPrefix(line, []byte("data: ")) {
		line = bytes.TrimPrefix(line, []byte("data: "))
		line = bytes.TrimSpace(line)
		if bytes.Equal(line, []byte("[DONE]")) {
			return
		}
	}
	var chunk streamChunk
	if err := json.Unmarshal(line, &chunk); err != nil {
		return
	}
	if chunk.Model != "" && r.responseModel == "" {
		r.responseModel = chunk.Model
	}
	if chunk.PromptEvalCount > 0 {
		r.promptTokens = chunk.PromptEvalCount
	}
	if chunk.EvalCount > 0 {
		r.outputTokens = chunk.EvalCount
	}
	if chunk.Usage != nil {
		if chunk.Usage.PromptTokens > 0 {
			r.promptTokens = chunk.Usage.PromptTokens
		}
		if chunk.Usage.CompletionTokens > 0 {
			r.outputTokens = chunk.Usage.CompletionTokens
		}
	}
	if r.recordPayloads && r.completionBuf.Len() < maxRecordedCompletionBytes {
		var toAppend string
		if chunk.Response != "" {
			toAppend = chunk.Response
		} else if chunk.Message != nil && chunk.Message.Content != "" {
			toAppend = chunk.Message.Content
		} else if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta.Content != "" {
				toAppend = chunk.Choices[0].Delta.Content
			} else if chunk.Choices[0].Message.Content != "" {
				toAppend = chunk.Choices[0].Message.Content
			}
		}
		if toAppend != "" {
			if r.completionBuf.Len()+len(toAppend) > maxRecordedCompletionBytes {
				remain := maxRecordedCompletionBytes - r.completionBuf.Len()
				if remain > 0 {
					r.completionBuf.WriteString(toAppend[:remain])
				}
				r.completionBuf.WriteString("... [TRUNCATED]")
			} else {
				r.completionBuf.WriteString(toAppend)
			}
		}
	}
}

func (r *telemetryBodyReader) finalize() {
	r.closeOnce.Do(func() {
		if r.span != nil && r.span.IsRecording() {
			if r.responseModel != "" {
				r.span.SetAttributes(attribute.String("gen_ai.response.model", r.responseModel))
			}
			if r.promptTokens > 0 {
				r.span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", r.promptTokens))
			}
			if r.outputTokens > 0 {
				r.span.SetAttributes(attribute.Int("gen_ai.usage.output_tokens", r.outputTokens))
			}
			if r.recordPayloads && r.completionBuf.Len() > 0 {
				comp := r.completionBuf.String()
				r.span.SetAttributes(
					attribute.String("gen_ai.completion", comp),
					attribute.String("output.value", comp),
				)
			}
		}
	})
}

func (r *telemetryBodyReader) Close() error {
	if len(r.lineBuf) > 0 {
		r.inspectLine(r.lineBuf)
		r.lineBuf = nil
	}
	r.finalize()
	return r.ReadCloser.Close()
}
