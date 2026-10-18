// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"nvpair-shared/telemetry"
)

func (p *Proxy) handleTelemetryConfigure(msg *Message) {
	revision, settings, err := telemetry.DecodeConfigureRequest(msg.Params)
	if err != nil {
		p.codec.RespondError(msg.ID, -32602, err.Error())
		return
	}
	revision, err = p.telemetry.Apply(context.Background(), revision, settings)
	if err != nil {
		p.codec.RespondError(msg.ID, -32000, "failed to configure telemetry")
		return
	}
	p.codec.Respond(msg.ID, telemetry.ConfigureResult{Revision: revision})
}

func (p *Proxy) handleTelemetryStatus(msg *Message) {
	p.codec.Respond(msg.ID, telemetry.NewStatusResult(p.telemetry.Status()))
}

func metadataAttribute(key, value string) attribute.KeyValue {
	value, _ = telemetry.TruncateUTF8(value, 256)
	return attribute.String(key, value)
}

// ReverseProxy otherwise copies the original client headers even without an
// injector. Context from local clients must never cross an engine/peer boundary.
func stripTraceContext(header http.Header) {
	header.Del("traceparent")
	header.Del("tracestate")
	header.Del("baggage")
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
	if in, ok := raw["input"]; ok {
		var str string
		if err := json.Unmarshal(in, &str); err == nil {
			return str
		}
		return string(in)
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

type telemetryBodyReader struct {
	io.ReadCloser
	span                trace.Span
	start               time.Time
	session             *telemetry.Session
	completionTruncated bool
	firstReadOnce       sync.Once
	closeOnce           sync.Once
	lineBuf             []byte
	completionBuf       strings.Builder
	promptTokens        int
	outputTokens        int
	responseModel       string
}

func newTelemetryBodyReader(body io.ReadCloser, span trace.Span, start time.Time, session *telemetry.Session) *telemetryBodyReader {
	return &telemetryBodyReader{
		ReadCloser: body,
		span:       span,
		start:      start,
		session:    session,
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

const maxTelemetryLineBytes = 4 << 20 // 4 MiB max line buffer for large non-streaming frames

func (r *telemetryBodyReader) observe(chunk []byte) {
	if !r.session.RecordPayloads() {
		r.completionBuf.Reset()
		r.completionTruncated = false
	}
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
		Text  string `json:"text"`
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
		r.responseModel, _ = telemetry.TruncateUTF8(chunk.Model, 256)
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
	if r.session.RecordPayloads() && !r.completionTruncated {
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
			} else if chunk.Choices[0].Text != "" {
				toAppend = chunk.Choices[0].Text
			}
		}
		part, truncated := telemetry.TruncateUTF8(toAppend, telemetry.MaxPayloadBytes-r.completionBuf.Len())
		r.completionBuf.WriteString(part)
		r.completionTruncated = truncated
	}
}

func (r *telemetryBodyReader) finalize() {
	r.closeOnce.Do(func() {
		defer r.completionBuf.Reset()
		if r.span != nil && r.span.IsRecording() {
			if r.responseModel != "" {
				r.span.SetAttributes(metadataAttribute("gen_ai.response.model", r.responseModel))
			}
			if r.promptTokens > 0 {
				r.span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", r.promptTokens))
			}
			if r.outputTokens > 0 {
				r.span.SetAttributes(attribute.Int("gen_ai.usage.output_tokens", r.outputTokens))
			}
			if r.session.RecordPayloads() && r.completionBuf.Len() > 0 {
				comp, _ := telemetry.TruncateUTF8(r.completionBuf.String(), telemetry.MaxPayloadBytes)
				r.span.SetAttributes(
					attribute.String("gen_ai.completion", comp),
					attribute.String("output.value", comp),
				)
				if r.completionTruncated {
					r.span.SetAttributes(attribute.Bool("gen_ai.completion.truncated", true))
				}
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
