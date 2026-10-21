// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"nvpair-shared/inferencemetrics"
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

	// requestStart is the instant the request reached the proxy. The live
	// progress frame measures its timings from here, so the elapsed time and
	// the time to the first byte are the client's own, not the engine's.
	requestStart time.Time

	// progressMu guards the live counters below. They are written by the
	// response copy goroutine and read by whoever asks the reader for a
	// snapshot, which need not be that goroutine.
	progressMu     sync.Mutex
	chunks         int64
	bytes          int64
	firstByteAt    time.Time
	firstContentAt time.Time

	// onProgress is called whenever the reader observes something new: the
	// first response byte, and every content chunk after it. It runs on the
	// response copy goroutine, so it must be cheap and must not block.
	onProgress func(LiveProgress)

	// measurementMu guards measurement, which is accumulated while the
	// response streams in and read once the stream is done. The measurement
	// is kept independently of the span: with telemetry disabled no span is
	// recording, but the engine's numbers are still reported.
	measurementMu sync.Mutex
	measurement   inferencemetrics.Measurement
}

func newTelemetryBodyReader(body io.ReadCloser, span trace.Span, start, requestStart time.Time, session *telemetry.Session) *telemetryBodyReader {
	return &telemetryBodyReader{
		ReadCloser:   body,
		span:         span,
		start:        start,
		requestStart: requestStart,
		session:      session,
	}
}

// LiveProgress is what the reader has measured of the response so far.
//
// Every number in it is exact. The bytes are the bytes the engine sent, the
// chunks are the content chunks that arrived, and the timings are measured
// instants — never an estimate. There is deliberately no token count and no
// rate: a chunk is not a token, and PAIR does not know the engine's token
// count until the response ends.
type LiveProgress struct {
	// Chunks is the number of content chunks received — chunks that carried
	// text, not every frame the engine sent.
	Chunks int64
	// Bytes is the number of response body bytes received.
	Bytes int64
	// TTFBMs is the time to the first response byte, in milliseconds.
	TTFBMs int64
	// TTFCMs is the time to the first content chunk, in milliseconds.
	TTFCMs int64
	// HasByte reports whether a response byte has arrived at all.
	HasByte bool
	// HasContent reports whether a content chunk has arrived at all.
	HasContent bool
}

func (r *telemetryBodyReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if n > 0 {
		r.recordFirstToken()
		r.recordBytes(n)
		r.observe(b[:n])
		// One notification per read, after the read's bytes have been
		// counted and its lines parsed: the snapshot a frame carries is
		// the state at the end of the read, not the state before it.
		r.notifyProgress()
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

// recordBytes counts the response bytes as they arrive and notes the instant
// the first byte came.
func (r *telemetryBodyReader) recordBytes(n int) {
	r.progressMu.Lock()
	if r.firstByteAt.IsZero() {
		r.firstByteAt = time.Now()
	}
	r.bytes += int64(n)
	r.progressMu.Unlock()
}

// recordContent counts a content chunk — a chunk that carried text — and
// notes the instant the first one came.
func (r *telemetryBodyReader) recordContent() {
	r.progressMu.Lock()
	r.chunks++
	if r.firstContentAt.IsZero() {
		r.firstContentAt = time.Now()
	}
	r.progressMu.Unlock()
}

// progressLocked returns the live snapshot. The caller holds progressMu.
func (r *telemetryBodyReader) progressLocked() LiveProgress {
	prog := LiveProgress{
		Chunks:  r.chunks,
		Bytes:   r.bytes,
		HasByte: !r.firstByteAt.IsZero(),
	}
	if prog.HasByte {
		prog.TTFBMs = r.firstByteAt.Sub(r.requestStart).Milliseconds()
	}
	if !r.firstContentAt.IsZero() {
		prog.HasContent = true
		prog.TTFCMs = r.firstContentAt.Sub(r.requestStart).Milliseconds()
	}
	return prog
}

// Progress returns the live counters of the response read so far.
func (r *telemetryBodyReader) Progress() LiveProgress {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	return r.progressLocked()
}

// notifyProgress hands the current snapshot to the progress callback, if one
// is installed. It runs on the response copy goroutine.
func (r *telemetryBodyReader) notifyProgress() {
	if r.onProgress == nil {
		return
	}
	r.onProgress(r.Progress())
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
		// The token counts are pointer-typed so a count the engine reported
		// as zero stays a measured zero, while a chunk that carries no
		// count at all leaves the field absent. oMLX reports a cached-token
		// count of zero on every request that missed the prefix cache, and
		// that zero is a measurement, not a missing value.
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
		// The phase durations oMLX reports on its terminal chunk, in
		// seconds. A phase the engine did not measure is absent, not zero.
		PromptEvalDuration  *float64 `json:"prompt_eval_duration"`
		GenerationDuration  *float64 `json:"generation_duration"`
		TimeToFirstToken    *float64 `json:"time_to_first_token"`
		TotalTime           *float64 `json:"total_time"`
		PromptTokensDetails *struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
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
		// The span attributes keep their own "more than zero" gate: a count
		// of zero says nothing about the span. The measurement below does
		// not share that gate — presence is the provenance there.
		if chunk.Usage.PromptTokens != nil && *chunk.Usage.PromptTokens > 0 {
			r.promptTokens = *chunk.Usage.PromptTokens
		}
		if chunk.Usage.CompletionTokens != nil && *chunk.Usage.CompletionTokens > 0 {
			r.outputTokens = *chunk.Usage.CompletionTokens
		}
		// The engine's own prefill/decode numbers, as oMLX reports them in
		// the usage block. Only what the engine reported is recorded: a
		// phase it did not measure stays absent instead of becoming a
		// zero, and a count it reported as zero stays that measured zero.
		r.measurementMu.Lock()
		if chunk.Usage.PromptTokens != nil {
			r.measurement.PrefillTokens = inferencemetrics.IntPtr(*chunk.Usage.PromptTokens)
		} else if r.promptTokens > 0 {
			r.measurement.PrefillTokens = inferencemetrics.IntPtr(r.promptTokens)
		}
		if chunk.Usage.CompletionTokens != nil {
			r.measurement.DecodeTokens = inferencemetrics.IntPtr(*chunk.Usage.CompletionTokens)
		} else if r.outputTokens > 0 {
			r.measurement.DecodeTokens = inferencemetrics.IntPtr(r.outputTokens)
		}
		if chunk.Usage.PromptTokensDetails != nil && chunk.Usage.PromptTokensDetails.CachedTokens != nil {
			r.measurement.CachedTokens = inferencemetrics.IntPtr(*chunk.Usage.PromptTokensDetails.CachedTokens)
		}
		setDurationMs(&r.measurement.PrefillMs, chunk.Usage.PromptEvalDuration)
		setDurationMs(&r.measurement.DecodeMs, chunk.Usage.GenerationDuration)
		setDurationMs(&r.measurement.TtftMs, chunk.Usage.TimeToFirstToken)
		setDurationMs(&r.measurement.TotalMs, chunk.Usage.TotalTime)
		r.measurementMu.Unlock()
	}
	// The chunk's text is read whether or not the telemetry session records
	// payloads: the live progress frame counts content chunks whatever the
	// telemetry settings are, and a chunk that carries text is what makes a
	// request a content chunk.
	content := chunkContent(chunk)
	if content != "" {
		r.recordContent()
	}
	if r.session.RecordPayloads() && !r.completionTruncated {
		toAppend := content
		part, truncated := telemetry.TruncateUTF8(toAppend, telemetry.MaxPayloadBytes-r.completionBuf.Len())
		r.completionBuf.WriteString(part)
		r.completionTruncated = truncated
	}
}

// chunkContent returns the text a streamed chunk carries, if any. A chunk that
// carries no text — a role-only delta, a usage block, a keep-alive — is not
// a content chunk.
func chunkContent(chunk streamChunk) string {
	if chunk.Response != "" {
		return chunk.Response
	}
	if chunk.Message != nil && chunk.Message.Content != "" {
		return chunk.Message.Content
	}
	if len(chunk.Choices) > 0 {
		if chunk.Choices[0].Delta.Content != "" {
			return chunk.Choices[0].Delta.Content
		}
		if chunk.Choices[0].Message.Content != "" {
			return chunk.Choices[0].Message.Content
		}
		if chunk.Choices[0].Text != "" {
			return chunk.Choices[0].Text
		}
	}
	return ""
}

// setDurationMs records an engine-reported duration, converted from the
// engine's seconds to whole milliseconds. A duration the engine did not
// report leaves the field absent.
func setDurationMs(dst **int64, seconds *float64) {
	if seconds == nil {
		return
	}
	*dst = inferencemetrics.Int64Ptr(int64(math.Round(*seconds * 1000)))
}

// Measurement returns the inference measurement parsed from the response
// stream so far. It is readable whether or not a span is recording, so the
// workload event carries the engine's numbers even with telemetry disabled.
func (r *telemetryBodyReader) Measurement() inferencemetrics.Measurement {
	r.measurementMu.Lock()
	defer r.measurementMu.Unlock()
	return r.measurement
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
