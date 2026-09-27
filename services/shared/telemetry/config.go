// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "time"

type Config struct {
	Enabled        bool          `json:"enabled"`
	Endpoint       string        `json:"endpoint"`
	ServiceName    string        `json:"service_name"`
	RecordPayloads bool          `json:"record_payloads"`
	BatchTimeout   time.Duration `json:"batch_timeout"`
}

func DefaultConfig(serviceName string) Config {
	return Config{
		Enabled:        true,
		Endpoint:       "localhost:4317",
		ServiceName:    serviceName,
		RecordPayloads: false,
		BatchTimeout:   time.Second,
	}
}
