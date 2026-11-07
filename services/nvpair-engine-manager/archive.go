// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const archiveMaxBytes int64 = 2 << 30
const archiveMaxFiles = 512

// ArchiveInstall unpacks a pinned tar.gz into a private sibling directory.
// Required names are relative to the stripped wrapper. Codesign identifies the
// executable whose existing signature must verify; it is never executed.
type ArchiveInstall struct {
	Required []string `json:"required"`
	Codesign string   `json:"codesign"`
}

func safeArchivePath(name string) bool {
	if name == "" || path.IsAbs(name) || strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func stripArchivePath(name string) (string, error) {
	if !safeArchivePath(name) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	_, rest, _ := strings.Cut(strings.TrimSuffix(name, "/"), "/")
	if rest == "" {
		return "", nil
	}
	rest = path.Clean(rest)
	if rest == "." {
		return "", nil
	}
	return rest, nil
}

// Links are created only after every regular file, so no archive entry can
// write through a link. A final resolution pass also rejects cycles, dangling
// links and links outside the staging root. Duplicate/case-alias names are
// refused rather than depending on the destination filesystem's case rules.
func extractTarGzip(ctx context.Context, source, root string, maxBytes int64, maxFiles int) error {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	bounded := &io.LimitedReader{R: gz, N: maxBytes + int64(maxFiles)*4096 + 1}
	tr := tar.NewReader(bounded)
	type link struct {
		name, target string
		hard         bool
	}
	var links []link
	seen := map[string]bool{}
	var total int64
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		count++
		if count > maxFiles {
			return fmt.Errorf("archive exceeds %d entries", maxFiles)
		}
		name, err := stripArchivePath(h.Name)
		if err != nil {
			return err
		}
		if h.Mode&06000 != 0 {
			return fmt.Errorf("archive file %q has setuid/setgid bits", h.Name)
		}
		for key := range h.PAXRecords {
			if strings.Contains(strings.ToLower(key), "com.apple.quarantine") {
				return fmt.Errorf("cannot install %s: it carries com.apple.quarantine; leave the file unchanged and obtain an unquarantined official archive", h.Name)
			}
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeSymlink && h.Typeflag != tar.TypeLink {
			return fmt.Errorf("unsupported archive entry %q", h.Name)
		}
		if name == "" {
			if h.Typeflag != tar.TypeDir {
				return fmt.Errorf("archive file %q has no path after stripping its wrapper", h.Name)
			}
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate archive path %q", name)
		}
		seen[key] = true
		dest := filepath.Join(root, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if h.Size < 0 || h.Size > maxBytes-total {
				return fmt.Errorf("archive exceeds %d extracted bytes", maxBytes)
			}
			total += h.Size
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(out, tr, h.Size)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			if !safeArchivePath(h.Linkname) {
				return fmt.Errorf("unsafe archive link %q", h.Name)
			}
			target := h.Linkname
			if h.Typeflag == tar.TypeLink {
				target, err = stripArchivePath(target)
				if err != nil || target == "" {
					return fmt.Errorf("unsafe archive hardlink %q", h.Name)
				}
			}
			links = append(links, link{name, target, h.Typeflag == tar.TypeLink})
		}
	}
	// Consume the gzip trailer too: a truncated or corrupt stream must not be
	// accepted merely because the tar end marker occurred before the trailer.
	if _, err := io.Copy(io.Discard, bounded); err != nil {
		return err
	}
	if bounded.N <= 0 {
		return fmt.Errorf("archive decompression limit exceeded")
	}
	for _, l := range links {
		dest := filepath.Join(root, filepath.FromSlash(l.name))
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		if l.hard {
			continue
		}
		if err := os.Symlink(filepath.FromSlash(l.target), dest); err != nil {
			return err
		}
	}
	for _, l := range links {
		if l.hard {
			continue
		}
		dest := filepath.Join(root, filepath.FromSlash(l.name))
		resolved, err := filepath.EvalSymlinks(dest)
		if err != nil || !pathWithinRoot(root, resolved) {
			return fmt.Errorf("archive link %q does not resolve inside the install directory", l.name)
		}
	}
	for _, l := range links {
		if !l.hard {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(l.target))
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("invalid archive hardlink %q", l.name)
		}
		if err := os.Link(target, filepath.Join(root, filepath.FromSlash(l.name))); err != nil {
			return err
		}
	}
	return nil
}

func installArchive(ctx context.Context, source, target, engine, digest string, spec *ArchiveInstall, verify func(context.Context, string, *ArchiveInstall) error) error {
	if err := rejectArchiveQuarantine(source); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	exists := false
	oldID := ""
	if _, err := os.Lstat(target); err == nil {
		if !installedByPAIR(target, target) {
			return fmt.Errorf("PAIR has no identity-bound record of installing %s; leaving it unchanged", target)
		}
		exists = true
		oldID = fileIdentity(target)
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".engine-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stage, err = filepath.EvalSymlinks(stage)
	if err != nil {
		return err
	}
	if err := extractTarGzip(ctx, source, stage, archiveMaxBytes, archiveMaxFiles); err != nil {
		return err
	}
	for _, name := range spec.Required {
		if !safeArchivePath(name) {
			return fmt.Errorf("invalid required archive path %q", name)
		}
		p := filepath.Join(stage, filepath.FromSlash(name))
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil || !pathWithinRoot(stage, resolved) {
			return fmt.Errorf("required archive file %q is missing or unsafe", name)
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("required archive file %q is missing or empty", name)
		}
	}
	if err := verify(ctx, stage, spec); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	marker := installMarker{Engine: engine, InstalledAt: time.Now().UTC().Format(time.RFC3339), Creates: target, Identity: fileIdentity(stage), ArtifactSHA256: digest}
	if marker.Identity == "" {
		return fmt.Errorf("cannot establish install directory identity")
	}
	if err := writeJSONAtomic(filepath.Join(stage, installMarkerName), marker); err != nil {
		return err
	}
	if exists {
		if oldID == "" || fileIdentity(target) != oldID || !installedByPAIR(target, target) {
			return fmt.Errorf("PAIR install identity changed during update; leaving it unchanged")
		}
		// Exchange is a single atomic filesystem operation. After success stage
		// holds the previous tree and its previous claim, removed by the defer.
		return swapArchiveDirectories(stage, target)
	}
	return renameArchiveNew(stage, target)
}

func (i *Install) validateArchive() error {
	if i.Fetch == nil || len(i.FetchVariants) > 0 || len(i.Script) > 0 || len(i.Run) > 0 || i.DiskImage != nil || i.Creates != "" || i.ModeOrDefault() != "user" {
		return fmt.Errorf("install.archive requires one pinned fetch in user mode, without script, run, disk_image or creates")
	}
	digest, err := hex.DecodeString(i.Fetch.SHA256)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("install.archive requires a SHA-256 checksum")
	}
	spec := i.Archive
	if len(spec.Required) == 0 || !safeArchivePath(spec.Codesign) {
		return fmt.Errorf("install.archive requires files and a safe codesign target")
	}
	found := false
	for _, name := range spec.Required {
		if !safeArchivePath(name) || path.Clean(name) != name || name == "." {
			return fmt.Errorf("invalid required archive path %q", name)
		}
		if name == spec.Codesign {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("install.archive codesign target must be required")
	}
	return nil
}

func (e *Executor) installManagedArchive(ctx context.Context, engine string, st *engineState) (err error) {
	defer func() {
		if err != nil {
			e.reportInstallFailed(engine, err)
		}
	}()
	inst := st.plat.Install
	if err := inst.validateArchive(); err != nil {
		return err
	}
	if ok, _ := e.Detect(engine); ok {
		if !installedByPAIR(st.installDir, st.installDir) {
			return fmt.Errorf("PAIR has no identity-bound record of installing %s; leaving it unchanged", engine)
		}
		data, readErr := os.ReadFile(installMarkerPath(st.installDir))
		var marker installMarker
		if readErr == nil && json.Unmarshal(data, &marker) == nil && marker.Engine == engine && strings.EqualFold(marker.ArtifactSHA256, inst.Fetch.SHA256) {
			if err := verifyArchivePlatform(ctx, st.installDir, inst.Archive); err != nil {
				return err
			}
			e.reporter.clear(installFailedID(engine))
			e.emitInstallProgress(engine, "already-installed", 100)
			return nil
		}
	}
	st.mu.Lock()
	port := st.port
	running := st.running
	st.mu.Unlock()
	presence := e.reconcilePresence(ctx, engine, st, fileExists(filepath.Join(st.installDir, inst.Archive.Codesign)), port, false)
	if running || presence.Occupied {
		return fmt.Errorf("stop %s before installing or updating it; its server port must be available", st.manifest.DisplayName)
	}
	e.emitInstallProgress(engine, "downloading", 0)
	download, err := e.download(ctx, engine, inst.Fetch)
	if err != nil {
		return err
	}
	defer os.Remove(download)
	e.emitInstallProgress(engine, "installing", 0)
	if err := installArchive(ctx, download, st.installDir, engine, inst.Fetch.SHA256, inst.Archive, verifyArchivePlatform); err != nil {
		return err
	}
	e.Detect(engine)
	e.reporter.clear(installFailedID(engine))
	e.emitInstallProgress(engine, "done", 100)
	e.emitState(engine)
	return nil
}
