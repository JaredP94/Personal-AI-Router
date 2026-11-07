// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"
)

// Install obtains the engine in user mode: download (checksum-verified)
// then run the declared command. No-op if already detected.
func (e *Executor) Install(ctx context.Context, engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.lockOperation()
	defer st.unlockOperation()
	if st.plat.Install != nil && st.plat.Install.Archive != nil {
		return e.installManagedArchive(ctx, engine, st)
	}
	if ok, _ := e.Detect(engine); ok {
		e.reporter.clear(installFailedID(engine))
		e.emitInstallProgress(engine, "already-installed", 100)
		return nil
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	presence := e.reconcilePresence(ctx, engine, st, false, port, false)
	if presence.Identified {
		// A healthy service is already present even though no managed binary
		// was detected. Treat it as an external installation and never fetch a
		// second copy. reconcilePresence emits the adopted state; the terminal
		// progress event clears any remaining install-progress UI.
		e.reporter.clear(installFailedID(engine))
		e.emitInstallProgress(engine, "already-installed", 100)
		return nil
	}
	if presence.Occupied && st.plat.Runtime.modeOrDefault() == "process" {
		err := fmt.Errorf("cannot install engine %q for port %d: the port is occupied by a service that did not identify as %s; use the existing service or choose another port", engine, port, st.manifest.DisplayName)
		e.reportInstallFailed(engine, err)
		return err
	}
	inst := st.plat.Install
	if inst == nil {
		return fmt.Errorf("engine %q has no install block for this platform", engine)
	}
	if inst.ModeOrDefault() == "admin" {
		err := fmt.Errorf("engine %q declares an admin install, which is refused (engine-manager is user-mode only)", engine)
		e.reportInstallFailed(engine, err)
		return err
	}
	if err := os.MkdirAll(st.installDir, 0o755); err != nil {
		return fmt.Errorf("create install dir: %w", err)
	}
	// Whether this install puts its Creates path in place, or finds the
	// user's own copy already there. Decided before anything runs.
	claims := true
	// createdID is the identity of what this install put at Creates, taken
	// as it was put there; the claim covers that bundle and nothing else.
	createdID := ""
	if inst.Creates != "" {
		if _, err := os.Lstat(expandPath(inst.Creates)); err == nil {
			claims = false
		}
	}

	vars := map[string]string{"install_dir": st.installDir}

	if len(inst.Script) > 0 {
		// Escape hatch: vendor-script install with no checksum. Logged
		// loudly so the weaker guarantee is never silent.
		slog.Warn("running UNPINNED script install (no checksum verification)", "engine", engine)
		e.emitInstallProgress(engine, "installing", 0)
		argv, err := resolveArgs(inst.Script, vars)
		if err != nil {
			e.reportInstallFailed(engine, err)
			return err
		}
		for i := range argv {
			argv[i] = expandPath(argv[i])
		}
		if err := e.runCommand(ctx, argv); err != nil {
			werr := fmt.Errorf("script install failed: %w", err)
			e.reportInstallFailed(engine, werr)
			return werr
		}
	} else {
		if len(inst.FetchVariants) > 0 || inst.Fetch != nil {
			fetch := inst.fetchFor(hostOSMajorVersion())
			if fetch == nil {
				err := fmt.Errorf("%s has no build for this version of the operating system", st.manifest.DisplayName)
				e.reportInstallFailed(engine, err)
				return err
			}
			e.emitInstallProgress(engine, "downloading", 0)
			dp, err := e.download(ctx, engine, fetch)
			if err != nil {
				e.reportInstallFailed(engine, err)
				return err
			}
			defer os.Remove(dp)
			vars["download"] = dp
			e.emitInstallProgress(engine, "verified", 0)
		}
		if inst.DiskImage != nil {
			e.emitInstallProgress(engine, "installing", 0)
			id, err := installAppFromDiskImage(ctx, vars["download"], inst.DiskImage)
			if err != nil {
				werr := fmt.Errorf("install from disk image failed: %w", err)
				e.reportInstallFailed(engine, werr)
				return werr
			}
			// An app that arrived while the image downloaded or copied was
			// left in place; it is the user's, whatever the earlier check saw.
			createdID = id
		}
		if len(inst.Run) > 0 {
			e.emitInstallProgress(engine, "installing", 0)
			args, err := resolveArgs(inst.Run, vars)
			if err != nil {
				e.reportInstallFailed(engine, err)
				return err
			}
			for i := range args {
				args[i] = expandPath(args[i])
			}
			if err := e.runCommand(ctx, args); err != nil {
				werr := fmt.Errorf("install command failed: %w", err)
				e.reportInstallFailed(engine, werr)
				return werr
			}
		}
	}

	if inst.Creates != "" && inst.DiskImage == nil && claims {
		// A command-driven install: take the identity as soon as it ran.
		createdID = fileIdentity(expandPath(inst.Creates))
	}
	if inst.Creates != "" {
		// Claim only if Creates is still exactly what this install put there.
		if createdID == "" || fileIdentity(expandPath(inst.Creates)) != createdID {
			claims = false
		}
	}
	if !e.waitDetect(engine, true, e.detectTimeout) {
		err := fmt.Errorf("engine %q was not detected after install", engine)
		e.reportInstallFailed(engine, err)
		return err
	}
	// Claim the install now that it is real and detected. A failure to record it
	// only costs the ability to uninstall later, so it must not fail the install.
	// An install that found the user's own copy in place claims nothing.
	if !claims {
		// Whatever an earlier claim covered is gone: the copy in place now is
		// not one this install created.
		clearInstallMarker(st.installDir)
		slog.Info("engine was already in place; not recording it as installed by PAIR", "engine", engine, "path", inst.Creates)
	} else if err := writeInstallMarker(st.installDir, engine, inst.Creates, createdID); err != nil {
		clearInstallMarker(st.installDir)
		slog.Warn("could not record that PAIR installed this engine; uninstall will decline it", "engine", engine, "err", err)
	}
	e.reporter.clear(installFailedID(engine))
	e.emitInstallProgress(engine, "done", 100)
	e.emitState(engine)
	return nil
}

// Uninstall runs the manifest's uninstall command (user-mode), stopping
// the engine first. No-op if the engine isn't currently detected.
func (e *Executor) Uninstall(ctx context.Context, engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.lockOperation()
	defer st.unlockOperation()
	if ok, _ := e.Detect(engine); !ok {
		// Already gone — by its own uninstaller, or by hand. Drop our ownership
		// claim with it: left behind, it would authorise removing a copy the
		// user installs later, which PAIR has no right to touch.
		clearInstallMarker(st.installDir)
		return e.setDesiredEnabled(engine, false)
	}
	un := st.plat.Uninstall
	if un == nil || (len(un.Run) == 0 && len(un.Remove) == 0) {
		return fmt.Errorf("engine %q has no uninstall defined for this platform", engine)
	}
	st.mu.Lock()
	binPath := st.binPath // resolved by the Detect at the top of Uninstall
	st.mu.Unlock()
	if st.plat.Runtime.modeOrDefault() == "command" && !uninstallerCoversCLI(un, binPath, st.installDir, runtime.GOOS) {
		err := fmt.Errorf("cannot uninstall engine %q: declared uninstaller does not cover the resolved CLI (%s); remove this installation using its original package manager or installer", engine, binPath)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: err.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return err
	}
	if st.plat.Runtime.modeOrDefault() == "process" && !isOwnedInstallPath(binPath, st.installDir) {
		err := fmt.Errorf("cannot uninstall engine %q: its executable is outside NVPAIR's managed install directory (%s)", engine, binPath)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: err.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return err
	}
	// A command-mode engine's CLI sits in a vendor location, so the coverage
	// test above can never vouch for ownership. Without the install marker
	// this could be the copy the user installed themselves, holding the data
	// they built up in it — decline rather than guess.
	if st.plat.Runtime.modeOrDefault() == "command" && !installedByPAIR(st.installDir, installCreates(st.plat)) {
		err := fmt.Errorf("cannot uninstall engine %q: NVIDIA PAIR has no record of installing it, so it may be your own installation; remove it with %s's own uninstaller", engine, st.manifest.DisplayName)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: err.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return err
	}
	if st.plat.Install != nil && st.plat.Install.Archive != nil && !installedByPAIR(st.installDir, st.installDir) {
		return fmt.Errorf("cannot uninstall %s: PAIR has no identity-bound record of installing it", engine)
	}
	// Detect proves only that the managed image exists, not whether a process is
	// serving. Reconcile first so doStop can either stop our exact managed
	// process (including a handle-less orphan) or refuse an external owner.
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	presence := e.reconcilePresence(ctx, engine, st, true, port, false)
	st.mu.Lock()
	ownedProcess := st.proc != nil
	st.mu.Unlock()
	if presence.Occupied && !presence.Identified && !ownedProcess {
		err := fmt.Errorf("cannot uninstall engine %q: port %d is occupied by an unidentified service", engine, port)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: err.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return err
	}
	if err := e.doStop(st, engine); err != nil {
		werr := fmt.Errorf("cannot uninstall engine %q: %w", engine, err)
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: werr.Error(), Severity: "error", Action: "none", EngineType: engine, Operation: "uninstall"})
		return werr
	}

	vars := st.pathVars()
	args, err := resolveArgs(un.Run, vars)
	if err != nil {
		return err
	}
	for i := range args {
		args[i] = expandPath(args[i])
	}
	targets, err := resolveArgs(un.Remove, vars)
	if err != nil {
		return err
	}
	// One attempt is the command followed by the declared removals, so a retry
	// re-runs the process-stopping command before trying the tree again — which
	// is what clears the case this retry loop exists for.
	attemptUninstall := func() error {
		if len(args) > 0 {
			if err := e.runCommand(ctx, args); err != nil {
				return err
			}
		}
		for _, target := range targets {
			if err := removeTreePreserving(target, st.modelsDir); err != nil {
				return err
			}
		}
		return nil
	}
	var runErr error
	for attempt := 1; attempt <= uninstallRetries; attempt++ {
		if runErr = attemptUninstall(); runErr == nil {
			break
		}
		if attempt < uninstallRetries {
			slog.Warn("uninstall command failed; retrying", "engine", engine, "attempt", attempt, "of", uninstallRetries, "err", runErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(uninstallBackoff):
			}
		}
	}
	// Detection is the verdict, not the delete errors. Removal is best-effort
	// inside the tree, so a file the engine still holds open leaves a harmless
	// remnant while the executable itself is gone — reporting that as a failure
	// would tell the user to retry an uninstall that already worked.
	if !e.waitDetect(engine, false, e.detectTimeout) {
		uerr := fmt.Errorf("engine %q still detected after uninstall", engine)
		if runErr != nil {
			uerr = fmt.Errorf("uninstall failed after %d attempts: %w", uninstallRetries, runErr)
		}
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: uerr.Error(), Severity: "error", Action: "retry", EngineType: engine, Operation: "uninstall"})
		return uerr
	}
	if runErr != nil {
		slog.Warn("engine removed, but some of its files could not be deleted", "engine", engine, "err", runErr)
	}
	st.mu.Lock()
	st.binPath = ""
	st.mu.Unlock()
	clearInstallMarker(st.installDir)
	e.reporter.clear(uninstallFailedID(engine))
	e.emitState(engine)
	return e.setDesiredEnabled(engine, false)
}

// maxDownloadBytes caps a single engine download (engine installers /
// archives run large — e.g. CUDA-bundled ~2 GB — but this guards against
// an unbounded/malicious response filling the disk). A var, not a const,
// so tests can lower it.
var maxDownloadBytes int64 = 8 << 30 // 8 GiB

// uninstallRetries / uninstallBackoff bound retrying a failed uninstall
// command. A command-mode engine's daemon (e.g. LM Studio's backend) can
// outlive its `stop` and briefly hold files open, so a straight rm hits
// "directory not empty"; a short retry lets the tree settle. Vars so tests
// can shrink the backoff.
var (
	uninstallRetries = 4
	uninstallBackoff = 3 * time.Second
)

// validateDownloadURL requires engine downloads over HTTPS, with plain
// http allowed only from loopback (the live tests serve the artifact from
// a local httptest server, and a user might run a LAN mirror). This is the
// only transport protection for an unpinned (no-sha256) fetch.
func validateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid download url %q: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "127.0.0.1" || h == "::1" || strings.EqualFold(h, "localhost") {
			return nil
		}
		return fmt.Errorf("download url %q must be https (plain http is allowed only from loopback)", raw)
	default:
		return fmt.Errorf("download url %q must use https", raw)
	}
}

func (e *Executor) download(ctx context.Context, engine string, f *Fetch) (string, error) {
	if err := validateDownloadURL(f.URL); err != nil {
		return "", err
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", f.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", f.URL, resp.StatusCode)
	}

	// Preserve the URL suffix so tools that require one (notably PowerShell's
	// -File, which accepts only .ps1 files) can execute the downloaded artifact
	// directly instead of evaluating remote content inline.
	tmp, err := os.CreateTemp("", "nvpair-engine-"+engine+"-*"+path.Ext(req.URL.Path))
	if err != nil {
		return "", err
	}
	pw := &progressWriter{total: resp.ContentLength, onPct: func(p int) {
		e.emitInstallProgress(engine, "downloading", p)
	}}
	h := sha256.New()
	// Read one byte past the cap so we can detect (and reject) overflow.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.TeeReader(io.LimitReader(resp.Body, maxDownloadBytes+1), pw))
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s: %w", f.URL, err)
	}
	if n > maxDownloadBytes {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s exceeds the %d-byte limit", f.URL, int64(maxDownloadBytes))
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if want := strings.TrimSpace(f.SHA256); want != "" {
		if !strings.EqualFold(sum, want) {
			os.Remove(tmp.Name())
			return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", f.URL, sum, want)
		}
	} else {
		// Unpinned download: bytes are not integrity-checked, only
		// transport-secured (HTTPS, enforced above) — the same weaker
		// guarantee as a `script` install. Logged loudly, and the computed
		// digest is surfaced so a manifest author can pin it later.
		slog.Warn("UNPINNED download: manifest has no sha256, integrity not verified",
			"engine", engine, "url", f.URL, "computed_sha256", sum)
	}
	return tmp.Name(), nil
}

// runCommand executes a manifest-declared argv (an install or uninstall
// step), hiding the console window on Windows; on failure it returns the
// combined output for diagnostics.
func (e *Executor) runCommand(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureSysProcAttr(cmd) // hide the console window on Windows
	out, err := ownedCombinedOutput(cmd, isControlCommand(ctx))
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// commandTail keeps memory bounded even when a command writes a very long line.
type commandTail struct {
	data     []byte
	maxBytes int
}

func (b *commandTail) Write(p []byte) (int, error) {
	n := len(p)
	if b.maxBytes <= 0 {
		return n, nil
	}
	if len(p) >= b.maxBytes {
		b.data = append(b.data[:0], p[len(p)-b.maxBytes:]...)
	} else {
		if overflow := len(b.data) + len(p) - b.maxBytes; overflow > 0 {
			b.data = b.data[:copy(b.data, b.data[overflow:])]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

// runCommandTail discards stdout and returns only the last maxLines stderr
// lines, capped to the last maxBytes bytes. Keeping at most maxBytes while
// streaming is equivalent to applying the byte cap after selecting the lines.
func (e *Executor) runCommandTail(ctx context.Context, argv []string, maxLines, maxBytes int) error {
	if len(argv) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureSysProcAttr(cmd) // hide the console window on Windows
	tail := &commandTail{maxBytes: maxBytes}
	cmd.Stdout = io.Discard
	cmd.Stderr = tail
	if err := runOwnedCommand(cmd); err != nil {
		lines := strings.SplitAfter(string(tail.data), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if maxLines <= 0 {
			lines = nil
		} else if len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}
		return fmt.Errorf("%v: %s", err, strings.Join(lines, ""))
	}
	return nil
}

func (e *Executor) reportInstallFailed(engine string, err error) {
	e.emitInstallFrame(engine, "failed", -1, err.Error())
	e.reporter.report(serviceError{
		ID: installFailedID(engine), Message: err.Error(),
		Severity: "error", Action: "retry", EngineType: engine, Operation: "install",
	})
}

// progressWriter counts bytes and reports integer-percent download
// progress; it satisfies io.Writer for use as a TeeReader sink.
type progressWriter struct {
	total int64
	read  int64
	last  int
	onPct func(int)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.read += int64(n)
	if p.total > 0 && p.onPct != nil {
		pct := int(p.read * 100 / p.total)
		if pct != p.last {
			p.last = pct
			p.onPct(pct)
		}
	}
	return n, nil
}

// installCreates is the platform's install.creates, or "" when it has none.
func installCreates(p *Platform) string {
	if p.Install == nil {
		return ""
	}
	return p.Install.Creates
}
