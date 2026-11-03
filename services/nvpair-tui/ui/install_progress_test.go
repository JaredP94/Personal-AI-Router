// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"testing"

	"nvpair-tui/rpc"
)

func TestInstallProgressRendersOnlyMeasuredPercent(t *testing.T) {
	for _, tc := range []struct{ payload, want string }{
		{`{"engine":"omlx","stage":"installing"}`, "install omlx: installing"},
		{`{"engine":"omlx","stage":"downloading","percent":0}`, "install omlx: downloading"},
		{`{"engine":"omlx","stage":"downloading","percent":42}`, "install omlx: downloading (42%)"},
		{`{"engine":"omlx","stage":"failed","percent":-1,"error":"copy failed"}`, "install omlx failed: copy failed"},
	} {
		v := newEnginesView(nil)
		v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:install-progress", Params: json.RawMessage(tc.payload)}})
		if v.status != tc.want {
			t.Errorf("%s: got %q, want %q", tc.payload, v.status, tc.want)
		}
	}
}
