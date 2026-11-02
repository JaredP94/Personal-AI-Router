// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !(darwin && arm64)

package main

type sensorCollector struct{}

func startSensorCollector(policy *SamplingPolicy) *sensorCollector { return nil }
func (c *sensorCollector) state() *sensorState                     { return nil }
func (c *sensorCollector) stop()                                   {}
