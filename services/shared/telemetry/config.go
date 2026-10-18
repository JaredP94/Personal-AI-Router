// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "errors"

// Settings is the broker-authorized tracing policy. The destination is always loopback.
type Settings struct {
	Enabled        bool
	CollectorPort  int
	RecordPayloads bool
}

func ValidateSettings(s Settings) error {
	if s.CollectorPort < 1 || s.CollectorPort > 65535 {
		return errors.New("collector port must be between 1 and 65535")
	}
	if s.RecordPayloads && !s.Enabled {
		return errors.New("payload recording requires telemetry to be enabled")
	}
	return nil
}
