// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testProcesses = struct {
	sync.Mutex
	dirs map[*testing.T]string
}{dirs: make(map[*testing.T]string)}

// Every fake invocation has a per-test owner, including copied executables and
// detached grandchildren. The fake registers its PID before doing any work.
func fakeEngineForTest(t *testing.T) string {
	t.Helper()
	dir := fakeProcessDir(t)
	bin := filepath.Join(dir, "fake-engine"+exeExt())
	if !fileExists(bin) {
		data, err := os.ReadFile(fakeEngineBin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, data, 0755); err != nil {
			t.Fatal(err)
		}
		registerFakeExecutable(t, bin, dir)
	}
	return bin
}

func fakeProcessDir(t *testing.T) string {
	t.Helper()
	testProcesses.Lock()
	defer testProcesses.Unlock()
	if dir := testProcesses.dirs[t]; dir != "" {
		return dir
	}
	dir, err := os.MkdirTemp(testProcessRoot, "owner-")
	if err != nil {
		t.Fatal(err)
	}
	testProcesses.dirs[t] = dir
	t.Cleanup(func() {
		if remaining := sweepFakeProcesses(dir); len(remaining) != 0 {
			t.Errorf("fake engines survived test cleanup: %+v", remaining)
		}
		testProcesses.Lock()
		delete(testProcesses.dirs, t)
		testProcesses.Unlock()
	})
	return dir
}

func registerFakeExecutable(t *testing.T, bin, owner string) {
	t.Helper()
	image, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(image)))
	owners := filepath.Join(testProcessRoot, "owners")
	if err := os.MkdirAll(owners, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owners, key), []byte(owner), 0600); err != nil {
		t.Fatal(err)
	}
}

type fakeProcessRecord struct {
	PID   int
	Image string
}

func trackedFakeProcesses(root string) []fakeProcessRecord {
	var alive []fakeProcessRecord
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasPrefix(entry.Name(), "process-") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var record fakeProcessRecord
		if json.Unmarshal(data, &record) == nil && record.PID > 0 && record.PID != os.Getpid() && pidAlive(record.PID) &&
			isOurEngineImage(testProcessImage(record.PID), record.Image) {
			alive = append(alive, record)
		}
		return nil
	})
	return alive
}

// killOwnedTestPID kills a saved fixture PID only while it still runs this
// test's own fake-engine copy, whose path is unique to the test. A PID the OS
// has since reused for another program is left alone. On Windows,
// killTestProcessGroup then revalidates ownership through a pinned handle.
func killOwnedTestPID(pid int, bin string) {
	if pid > 0 && pid != os.Getpid() && isOurEngineImage(testProcessImage(pid), bin) {
		killTestProcessGroup(pid)
	}
}

func sweepFakeProcesses(root string) []fakeProcessRecord {
	deadline := time.Now().Add(3 * time.Second)
	for {
		alive := trackedFakeProcesses(root)
		if len(alive) == 0 {
			return nil
		}
		for _, record := range alive {
			killTestProcessGroup(record.PID)
		}
		if time.Now().After(deadline) {
			return trackedFakeProcesses(root)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
