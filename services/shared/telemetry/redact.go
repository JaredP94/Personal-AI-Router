// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "strings"

var sensitiveHeaderKeys = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"x-pair-pin":          {},
	"x-api-key":           {},
	"proxy-auth":          {},
	"proxy-authorization": {},
	"set-cookie":          {},
}

func RedactHeaders(headers map[string][]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		lower := strings.ToLower(k)
		if _, sensitive := sensitiveHeaderKeys[lower]; sensitive {
			out[k] = "[REDACTED]"
		} else if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func FormatPayloadAttributes(recordPayloads bool, prompt, completion string) map[string]string {
	out := make(map[string]string)
	if !recordPayloads {
		return out
	}
	if prompt != "" {
		out["input.value"] = prompt
		out["gen_ai.prompt"] = prompt
	}
	if completion != "" {
		out["output.value"] = completion
		out["gen_ai.completion"] = completion
	}
	return out
}
