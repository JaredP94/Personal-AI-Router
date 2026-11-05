// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestProcessHelpersRequireIsolation prevents new launch helpers from bypassing
// the sole runtime command constructor. goBuild deliberately keeps the real Go
// cache environment: it builds, but never executes, an application binary.
func TestProcessHelpersRequireIsolation(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		execNames := map[string]bool{}
		osNames := map[string]bool{}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os"` {
				name := "os"
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if name == "." {
					t.Errorf("%s: dot import bypasses isolation guard", path)
				}
				osNames[name] = true
			}
			if imp.Path.Value == `"os/exec"` {
				name := "exec"
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if name == "." {
					t.Errorf("%s: dot import bypasses isolation guard", path)
				}
				execNames[name] = true
			}
		}
		// *exec.Cmd annotations are harmless; values and constructors must
		// come from the isolated factory, including new(exec.Cmd) or literals.
		pointerTypes := map[*ast.SelectorExpr]bool{}
		ast.Inspect(file, func(node ast.Node) bool {
			if ptr, ok := node.(*ast.StarExpr); ok {
				if sel, ok := ptr.X.(*ast.SelectorExpr); ok {
					pointerTypes[sel] = true
				}
			}
			return true
		})
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			allowed := isFunc && path == "main_test.go" && (fn.Name.Name == "goBuild" || fn.Name.Name == "testCommandInDir")
			envAllowed := allowed || (isFunc && path == "main_test.go" && fn.Name.Name == "addTestEnv")
			ast.Inspect(decl, func(node ast.Node) bool {
				if assign, ok := node.(*ast.AssignStmt); ok && !envAllowed {
					for _, lhs := range assign.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
							t.Errorf("%s: use addTestEnv instead of replacing isolated Env", fset.Position(sel.Pos()))
						}
					}
				}
				sel, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if ok && osNames[pkg.Name] && sel.Sel.Name == "StartProcess" {
					t.Errorf("%s: use testCommand instead of os.StartProcess", fset.Position(sel.Pos()))
				}
				if ok && execNames[pkg.Name] && !pointerTypes[sel] && !allowed {
					t.Errorf("%s: use testCommand/testCommandInDir instead of os/exec.%s", fset.Position(sel.Pos()), sel.Sel.Name)
				}
				return true
			})
		}
	}
}

// Ported from NVIDIA/Personal-AI-Router@937cf6c, using this fork's
// standalone LM Studio proxy rather than the upstream unified proxy facade.
func TestLMStudioChildPersistsUnderPrivateBase(t *testing.T) {
	root := t.TempDir()
	cmd := testCommandInDir(t, root, lmstudioProxyBin, "--port", "0", "--ignore-persisted-port")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	msgs := startMsgReader(stdout)
	waitForMethod(t, msgs, "ready", 10*time.Second)
	requestID := 0
	port, err := retryPortBind(t, func() int { return freePort(t) }, func(port int) error {
		requestID++
		sendLine(t, stdin, map[string]any{
			"jsonrpc": "2.0", "id": requestID, "method": "set-port",
			"params": map[string]int{"port": port},
		})
		resp := waitForResponseID(t, msgs, requestID, 5*time.Second)
		if resp.Error != nil {
			return resp.Error
		}
		return nil
	})
	if err != nil {
		t.Fatalf("set-port: %v", err)
	}
	base := root
	if runtime.GOOS == "darwin" {
		base = filepath.Join(root, "Library", "Application Support")
	}
	path := filepath.Join(base, "Nvidia Corporation", "Personal AI Router", "lmstudio-proxy-port.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("child did not persist inside its private root: %v", err)
	}
	var saved struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Port != port {
		t.Fatalf("saved port = %d, want %d", saved.Port, port)
	}
}

func TestCommandEnvironmentIsPerTest(t *testing.T) {
	var roots []string
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			cmd := testCommand(t, "not-executed")
			values := map[string]string{}
			for _, entry := range cmd.Environ() {
				key, value, _ := strings.Cut(entry, "=")
				values[key] = value
			}
			root := values["HOME"]
			if root == "" || root == os.Getenv("HOME") {
				t.Fatal("HOME is not isolated")
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA", "USERPROFILE"} {
				if values[key] != root {
					t.Errorf("%s = %q, want %q", key, values[key], root)
				}
			}
			roots = append(roots, root)
		})
	}
	if len(roots) == 2 && roots[0] == roots[1] {
		t.Fatal("tests share an app-data root")
	}
}
