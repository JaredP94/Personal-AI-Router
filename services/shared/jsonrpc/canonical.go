// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// checkCanonical walks only the top-level object with json.Decoder.Token (member
// names are decoded, so escapes like "s\u0065q" count as "seq"). It returns a
// *DecodeError if a reserved name appears twice, or if any top-level name
// matches a reserved name case-insensitively but not exactly. Non-object or
// invalid JSON is left to the existing unmarshal error.
func checkCanonical(line []byte) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil
	}

	seen := make(map[string]bool, len(ReservedMembers))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil
		}

		for _, reserved := range ReservedMembers {
			if strings.EqualFold(key, reserved) {
				if key != reserved {
					return &DecodeError{fmt.Errorf("non-canonical envelope member %q (must be %q)", key, reserved)}
				}
				if seen[reserved] {
					return &DecodeError{fmt.Errorf("duplicate envelope member %q", reserved)}
				}
				seen[reserved] = true
				break
			}
		}

		if err := skipValue(dec); err != nil {
			return nil
		}
	}
	return nil
}

// skipValue advances dec past the value of an object member.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return fmt.Errorf("unexpected delimiter: %v", delim)
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
