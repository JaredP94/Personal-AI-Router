// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type conformanceRow struct {
	Name   string  `json:"name"`
	Frame  string  `json:"frame"`
	Expect string  `json:"expect"`
	Kind   *string `json:"kind"`
	Epoch  *string `json:"epoch"`
	Seq    *string `json:"seq"`
}

func TestEnvelopeConformanceCorpus(t *testing.T) {
	path := filepath.Join("testdata", "envelope-conformance.jsonl")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open conformance corpus: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var row conformanceRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d: invalid JSON in corpus row: %v", lineNum, err)
		}

		t.Run(row.Name, func(t *testing.T) {
			codec := readCodec(strings.NewReader(row.Frame + "\n"))
			msg, err := codec.Read()

			switch row.Expect {
			case "accept":
				if err != nil {
					t.Fatalf("expected accept, got error: %v", err)
				}
				var kind string
				switch {
				case msg.IsNotification():
					kind = "notification"
				case msg.IsRequest():
					kind = "request"
				case msg.IsResponse():
					kind = "response"
				default:
					t.Fatalf("message classified as neither notification, request, nor response: %+v", msg)
				}

				if row.Kind == nil {
					t.Errorf("expected null kind, got %q", kind)
				} else if kind != *row.Kind {
					t.Errorf("kind = %q, want %q", kind, *row.Kind)
				}

				if row.Epoch == nil {
					if msg.Epoch != nil {
						t.Errorf("epoch = %q, want nil", *msg.Epoch)
					}
				} else {
					if msg.Epoch == nil {
						t.Errorf("epoch = nil, want %q", *row.Epoch)
					} else if *msg.Epoch != *row.Epoch {
						t.Errorf("epoch = %q, want %q", *msg.Epoch, *row.Epoch)
					}
				}

				if row.Seq == nil {
					if msg.Seq != nil {
						t.Errorf("seq = %d, want nil", *msg.Seq)
					}
				} else {
					if msg.Seq == nil {
						t.Errorf("seq = nil, want %s", *row.Seq)
					} else {
						gotSeq := strconv.FormatUint(*msg.Seq, 10)
						if gotSeq != *row.Seq {
							t.Errorf("seq = %s, want %s", gotSeq, *row.Seq)
						}
					}
				}

			case "decode-error":
				if err == nil {
					t.Fatalf("expected decode-error, got accepted message: %+v", msg)
				}
				var decodeErr *DecodeError
				if !errors.As(err, &decodeErr) {
					t.Fatalf("expected *DecodeError, got %T: %v", err, err)
				}
				var mismatchErr *ProtocolMismatchError
				if errors.As(err, &mismatchErr) {
					t.Fatalf("got *ProtocolMismatchError for decode-error expectation: %v", err)
				}

			case "mismatch":
				if err == nil {
					t.Fatalf("expected mismatch, got accepted message: %+v", msg)
				}
				var mismatchErr *ProtocolMismatchError
				if !errors.As(err, &mismatchErr) {
					t.Fatalf("expected *ProtocolMismatchError, got %T: %v", err, err)
				}
				var decodeErr *DecodeError
				if errors.As(err, &decodeErr) {
					t.Fatalf("got *DecodeError for mismatch expectation: %v", err)
				}

			default:
				t.Fatalf("unknown expect value %q in corpus row %q", row.Expect, row.Name)
			}
		})
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error reading corpus: %v", err)
	}
}
