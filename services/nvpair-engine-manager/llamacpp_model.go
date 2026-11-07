// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// llamaModelPath admits one existing regular GGUF inside the managed cache.
// Resolve both the cache and candidate before the containment check, including
// symlinks in intermediate directories. Return the canonical absolute path so
// the child never interprets a relative path against its working directory.
func llamaModelPath(cache, value string) (string, error) {
	fail := func() (string, error) {
		return "", fmt.Errorf("model must be an existing regular .gguf file inside the PAIR llama.cpp model cache")
	}
	if strings.Contains(value, ":Q4_K_M") {
		file, err := recordedLlamaModelPath(cache, value)
		if err != nil {
			return "", err
		}
		value = file
	}
	if cache == "" || value == "" || strings.Contains(value, "://") {
		return fail()
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && !filepath.IsAbs(value) {
		return fail()
	}
	root, err := filepath.Abs(cache)
	if err != nil {
		return fail()
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fail()
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return fail()
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fail()
	}
	if _, err := llamaFileAlias(rel); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(path), ".gguf") {
		return fail()
	}
	if filepath.Dir(path) == root {
		dir, err := openLlamaCache(root, false)
		if err != nil {
			return "", err
		}
		defer dir.Close()
		file, err := llamaCacheOpen(dir, filepath.Base(path), false)
		if err != nil {
			return "", err
		}
		defer file.Close()
		if _, err := llamaRecordedModelAlias(dir, filepath.Base(path), file); err != nil {
			return "", err
		}
	}
	return path, nil
}

const llamaModelDescriptorPath = "/dev/fd/3" // first exec.Cmd.ExtraFiles entry

var llamaSplitName = regexp.MustCompile(`(?i)-[0-9]{5}-of-[0-9]{5}\.gguf$`)
var llamaAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*$`)

// Keep API identity beside the verified descriptor. Recorded aliases come from
// the manifest read through the same pinned cache directory as the model open.
type llamaModelFile struct {
	*os.File
	alias string
}

func llamaFileAlias(rel string) (string, error) {
	alias := strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
	if !llamaAlias.MatchString(alias) {
		return "", fmt.Errorf("llama.cpp model alias must use only ASCII letters, digits, underscore, dot, colon, slash or hyphen, starting with a letter or digit")
	}
	return alias, nil
}

// b12171 opens this path repeatedly (GGUF metadata, fitting, then llama_file
// reads/mmap). /dev/fd duplicates the inherited descriptor on macOS, so every
// open stays on the verified inode even if the original pathname is replaced.
func prepareLlamaModelArgs(args []string, cache string) ([]string, []*os.File, error) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--model" {
			continue
		}
		if llamaSplitName.MatchString(args[i+1]) {
			return nil, nil, fmt.Errorf("split/multi-file GGUF models are not supported yet; select a single-file GGUF")
		}
		file, err := openLlamaModel(cache, args[i+1])
		if err != nil {
			return nil, nil, err
		}
		childArgs := append([]string(nil), args...)
		childArgs[i+1] = llamaModelDescriptorPath
		childArgs = append(childArgs, "--alias", file.alias)
		return childArgs, []*os.File{file.File}, nil
	}
	return args, nil, nil
}

// Reject renamed shards as well as conventional split filenames. Read only
// bounded GGUF v2/v3 metadata, never tensors; arrays cannot nest in GGUF.
// This is not a model validator: llama.cpp still validates tensors and shape.
func rejectSplitGGUF(file *os.File) error {
	invalid := fmt.Errorf("model must have valid single-file GGUF v2/v3 metadata (maximum 64 MiB)")
	r := bufio.NewReader(io.NewSectionReader(file, 0, 64<<20))
	var header struct {
		Magic   [4]byte
		Version uint32
		Tensors uint64
		Keys    uint64
	}
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil || string(header.Magic[:]) != "GGUF" || (header.Version != 2 && header.Version != 3) || header.Keys > 1<<20 {
		return invalid
	}
	readString := func() (string, error) {
		var size uint64
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil || size > 1024 {
			return "", invalid
		}
		data := make([]byte, size)
		_, err := io.ReadFull(r, data)
		return string(data), err
	}
	skipString := func() error {
		var size uint64
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil || size > 64<<20 {
			return invalid
		}
		_, err := io.CopyN(io.Discard, r, int64(size))
		return err
	}
	// GGUF enum: u8/i8/u16/i16/u32/i32/f32/bool/string/array/u64/i64/f64.
	sizes := [...]int64{1, 1, 2, 2, 4, 4, 4, 1, 0, 0, 8, 8, 8}
	for k := uint64(0); k < header.Keys; k++ {
		key, err := readString()
		if err != nil {
			return invalid
		}
		if strings.HasPrefix(key, "split.") {
			return fmt.Errorf("split/multi-file GGUF models are not supported yet; select a single-file GGUF")
		}
		var kind uint32
		if err := binary.Read(r, binary.LittleEndian, &kind); err != nil || kind >= uint32(len(sizes)) {
			return invalid
		}
		count := uint64(1)
		if kind == 9 {
			if err := binary.Read(r, binary.LittleEndian, &kind); err != nil || kind >= uint32(len(sizes)) || kind == 9 {
				return invalid
			}
			if err := binary.Read(r, binary.LittleEndian, &count); err != nil || count > 1<<20 {
				return invalid
			}
		}
		if kind == 8 {
			for n := uint64(0); n < count; n++ {
				if err := skipString(); err != nil {
					return invalid
				}
			}
		} else if _, err := io.CopyN(io.Discard, r, int64(count)*sizes[kind]); err != nil {
			return invalid
		}
	}
	return nil
}

// b12171 /props reports role=router without a model, but model_path in
// single-model mode (server-context.cpp:get_res_props). Preserve identity
// checking in both modes, without mutating shared manifest probe pointers.
func llamaRuntimeProbes(rt Runtime, installDir string) Runtime {
	if !rt.EditableLaunch.llamaCPP() || rt.LaunchArgs == nil {
		return rt
	}
	args, err := normalizeLlamaOptions(*rt.LaunchArgs, installDir+"-models")
	if err != nil {
		return rt
	} // the launch path reports invalid saved settings
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--model" {
			continue
		}
		copyProbe := func(p *Probe) *Probe {
			if p == nil {
				return nil
			}
			copy := *p
			copy.JSONMatch = &ProbeJSONMatch{Field: "model_path", Value: llamaModelDescriptorPath}
			return &copy
		}
		rt.Ready, rt.Health = copyProbe(rt.Ready), copyProbe(rt.Health)
		break
	}
	return rt
}
