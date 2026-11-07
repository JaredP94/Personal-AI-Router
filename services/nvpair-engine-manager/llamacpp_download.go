// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const llamaManifestName = ".pair-models.json"

type llamaOwnedModel struct {
	ID        string `json:"id"`
	File      string `json:"file"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	FinalHost string `json:"finalHost"`
}
type llamaCacheRecord struct {
	llamaOwnedModel
	Identity string `json:"fileIdentity"`
}
type llamaCacheManifest struct {
	Models []llamaCacheRecord `json:"models"`
}

func llamaOwnedFilename(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:]) + ".gguf"
}
func readLlamaManifest(dir *os.File) (llamaCacheManifest, error) {
	out := llamaCacheManifest{Models: []llamaCacheRecord{}}
	f, err := llamaCacheOpen(dir, llamaManifestName, false)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return out, fmt.Errorf("invalid model ownership manifest")
	}
	if json.Unmarshal(data, &out) != nil || len(out.Models) > 4096 {
		return out, fmt.Errorf("invalid model ownership manifest")
	}
	seen := map[string]bool{}
	for _, m := range out.Models {
		if _, err := llamaRepoID(m.ID); err != nil || m.File != llamaOwnedFilename(m.ID) || m.Size <= 0 || m.Size > llamaMaxModelSize || !llamaSHA.MatchString(m.SHA256) || seen[m.ID] || m.Identity == "" {
			return out, fmt.Errorf("invalid model ownership record")
		}
		seen[m.ID] = true
	}
	return out, nil
}
func llamaTempName() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("system random unavailable")
	}
	return ".pair-" + hex.EncodeToString(bytes[:]) + ".tmp"
}
func writeLlamaManifest(dir *os.File, m llamaCacheManifest) error {
	data, err := json.Marshal(m)
	if err != nil || len(data) > 1<<20 {
		return fmt.Errorf("model ownership manifest full")
	}
	name := llamaTempName()
	f, err := llamaCacheOpen(dir, name, true)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = llamaCacheRemove(dir, name) }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = llamaCacheRename(dir, name, llamaManifestName); err != nil {
		return err
	}
	return dir.Sync()
}

// A reservation is a state flag, not a lock held across filesystem/network I/O.
// Reject concurrent cache mutations rather than queuing an unbounded wait.
func (e *Executor) reserveLlamaCache(deleting bool) (*engineState, func(), error) {
	st, err := e.state("llamacpp")
	if err != nil {
		return nil, nil, err
	}
	st.mu.Lock()
	if st.llamaCacheBusy || (deleting && (st.running || st.opDone != nil)) {
		st.mu.Unlock()
		return nil, nil, fmt.Errorf("llama.cpp model cache busy or engine in use; stop the engine before deleting")
	}
	st.llamaCacheBusy = true
	st.mu.Unlock()
	return st, func() { st.mu.Lock(); st.llamaCacheBusy = false; st.mu.Unlock() }, nil
}
func (e *Executor) pullLlamaModel(ctx context.Context, id string) (result json.RawMessage, retErr error) {
	if _, err := llamaRepoID(id); err != nil {
		return nil, err
	}
	st, release, err := e.reserveLlamaCache(false)
	if err != nil {
		return nil, err
	}
	defer release()
	dir, err := openLlamaCache(st.installDir+"-models", true)
	if err != nil {
		return nil, fmt.Errorf("open model cache: %w", err)
	}
	defer dir.Close()
	manifest, err := readLlamaManifest(dir)
	if err != nil {
		return nil, err
	}
	for _, m := range manifest.Models {
		if m.ID == id {
			return nil, fmt.Errorf("model already recorded; delete it before downloading again")
		}
	}
	row, downloadURL, err := e.llamaHub.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	free, err := e.llamaFreeSpace(dir)
	if err != nil || free < uint64(row.Size)+(256<<20) {
		return nil, fmt.Errorf("insufficient free space for model and 256 MiB reserve")
	}
	final := llamaOwnedFilename(id)
	if f, err := llamaCacheOpen(dir, final, false); err == nil {
		_ = f.Close()
		return nil, fmt.Errorf("destination already exists; unrecorded files are never replaced")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	temp := llamaTempName()
	f, err := llamaCacheOpen(dir, temp, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = f.Close()
		if err := llamaCacheRemove(dir, temp); err != nil && !os.IsNotExist(err) {
			result = nil
			retErr = fmt.Errorf("model transfer stopped but its temporary file could not be removed")
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid model download request")
	}
	resp, err := e.llamaHub.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("model transfer failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != row.Size {
		return nil, fmt.Errorf("model size mismatch")
	}
	hash := sha256.New()
	reader := io.LimitReader(resp.Body, row.Size+1)
	buf := make([]byte, 128<<10)
	var total int64
	last := -1
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > row.Size {
				return nil, fmt.Errorf("model exceeds expected size")
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return nil, err
			}
			_, _ = hash.Write(buf[:n])
			pct := int(total * 100 / row.Size)
			if pct != last {
				last = pct
				e.emitPullUpdate(ctx, ProgressEvent{Stage: "downloading", Percent: pct})
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("model transfer interrupted")
		}
	}
	if total != row.Size {
		return nil, fmt.Errorf("model size mismatch")
	}
	if hex.EncodeToString(hash.Sum(nil)) != row.SHA256 {
		return nil, fmt.Errorf("model checksum mismatch")
	}
	if err := rejectSplitGGUF(f); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	// Keep the verified descriptor open through rename and ownership commit.
	// A replacement at the final pathname must never acquire this claim.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := e.llamaCommit(dir, temp, final); err != nil {
		return nil, err
	}
	record := llamaOwnedModel{ID: id, File: final, Size: row.Size, SHA256: row.SHA256, FinalHost: resp.Request.URL.Hostname()}
	identity := llamaCacheIdentity(f)
	if identity == "" {
		return nil, fmt.Errorf("cannot identify committed model")
	}
	manifest.Models = append(manifest.Models, llamaCacheRecord{llamaOwnedModel: record, Identity: identity})
	if err := writeLlamaManifest(dir, manifest); err != nil {
		// This operation created the final file; do not leave an unowned success.
		candidate, openErr := llamaCacheOpen(dir, final, false)
		if openErr == nil {
			if llamaCacheIdentity(candidate) == identity {
				_ = llamaCacheRemove(dir, final)
			}
			_ = candidate.Close()
		}
		return nil, err
	}
	return json.Marshal(record)
}
func (e *Executor) listLlamaModels(ctx context.Context) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := e.state("llamacpp")
	if err != nil {
		return nil, err
	}
	dir, err := openLlamaCache(st.installDir+"-models", false)
	if os.IsNotExist(err) {
		return json.RawMessage(`{"models":[]}`), nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	manifest, err := readLlamaManifest(dir)
	if err != nil {
		return nil, err
	}
	models := []llamaOwnedModel{}
	// Verify the manifest against a bounded no-follow scan of the flat cache.
	// Manually supplied single-file GGUFs remain selectable, but never deletable.
	entries, err := dir.ReadDir(4097)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, fmt.Errorf("model cache has too many entries")
	}
	recorded := map[string]bool{}
	for _, m := range manifest.Models {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		recorded[m.File] = true
		f, err := llamaCacheOpen(dir, m.File, false)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		valid := err == nil && info.Size() == m.Size && llamaCacheIdentity(f) == m.Identity && rejectSplitGGUF(f) == nil
		_ = f.Close()
		if valid {
			models = append(models, m.llamaOwnedModel)
		}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if recorded[name] || !validLlamaFile(name) {
			continue
		}
		f, err := llamaCacheOpen(dir, name, false)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		valid := err == nil && info.Size() > 0 && info.Size() <= llamaMaxModelSize && rejectSplitGGUF(f) == nil
		_ = f.Close()
		if valid {
			models = append(models, llamaOwnedModel{ID: name, File: name, Size: info.Size()})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"models": models})
}
func (e *Executor) deleteLlamaModel(ctx context.Context, id string) (json.RawMessage, error) {
	if _, err := llamaRepoID(id); err != nil {
		return nil, err
	}
	st, release, err := e.reserveLlamaCache(true)
	if err != nil {
		return nil, err
	}
	defer release()
	dir, err := openLlamaCache(st.installDir+"-models", false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	manifest, err := readLlamaManifest(dir)
	if err != nil {
		return nil, err
	}
	for i, m := range manifest.Models {
		if m.ID != id {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		f, err := llamaCacheOpen(dir, m.File, false)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		identity := llamaCacheIdentity(f)
		_ = f.Close()
		if err != nil || info.Size() != m.Size || identity != m.Identity {
			return nil, fmt.Errorf("recorded model changed")
		}
		if err := llamaCacheRemove(dir, m.File); err != nil {
			return nil, err
		}
		manifest.Models = append(manifest.Models[:i], manifest.Models[i+1:]...)
		if err := writeLlamaManifest(dir, manifest); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	return nil, fmt.Errorf("model was not downloaded by PAIR")
}

// Idle transport reads are bounded independently of the 30-minute operation.
// Errors returned to callers intentionally omit URLs and remote body contents.
type llamaIdleConn struct{ net.Conn }

func (c *llamaIdleConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}
func llamaDial(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &llamaIdleConn{c}, nil
}

// Resolve a managed selection by ID or its installed filename. Never promote
// a manually copied file to a PAIR-owned model through launch settings.
func recordedLlamaModelPath(cache, value string) (string, error) {
	dir, err := openLlamaCache(cache, false)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	manifest, err := readLlamaManifest(dir)
	if err != nil {
		return "", err
	}
	for _, m := range manifest.Models {
		if value == m.ID || value == m.File || value == strings.TrimSuffix(dir.Name(), "/")+"/"+m.File {
			return m.File, nil
		}
	}
	return "", fmt.Errorf("model must be a PAIR-recorded completed llama.cpp download")
}
