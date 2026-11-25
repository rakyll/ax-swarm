// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseArgsAndDefaults(t *testing.T) {
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("0123456789abcdef", 4)
	if err := os.WriteFile(filepath.Join(binDir, "ax-runner.digest"), []byte(digest+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeAX := filepath.Join(binDir, "ax")
	if err := os.WriteFile(fakeAX, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AX_BIN", "")
	t.Setenv("AX_TASK_IMAGE", "")
	t.Setenv("AX_SERVER", "")
	t.Setenv("AGY_BIN", "")

	var stderr bytes.Buffer
	cfg, err := parseArgs([]string{
		"--workspace", `{"setup":{"git":[{"repo":"https://github.com/chalk/chalk"},{"repo":"https://github.com/google/go-cmp"}],"commands":["npm ci"]}}`,
		"--allow-host", "api.example.com",
		"--parallel", "4",
		"--task-timeout", "900",
		"--model", "gemini-4-neon-high",
		"--effort", "high",
		"--mode", "auto",
		"--keep",
		"inspect", "the", "codebase",
	}, tmp, &stderr)
	if err != nil {
		t.Fatalf("parseArgs() error: %v", err)
	}
	if cfg.Goal != "inspect the codebase" {
		t.Fatalf("Goal = %q, want %q", cfg.Goal, "inspect the codebase")
	}
	if !slices.Equal(cfg.AllowHosts, []string{"api.example.com"}) {
		t.Fatalf("AllowHosts = %v", cfg.AllowHosts)
	}
	if cfg.Image != "localhost:5001/ax-runner@"+digest {
		t.Fatalf("Image = %q", cfg.Image)
	}
	if cfg.AXBin != fakeAX {
		t.Fatalf("AXBin = %q, want %q", cfg.AXBin, fakeAX)
	}
	if cfg.SetupTimeout != 900*time.Second {
		t.Fatalf("SetupTimeout = %s, want 15m0s", cfg.SetupTimeout)
	}
	if err := validateConfig(&cfg, tmp); err != nil {
		t.Fatalf("validateConfig() unexpected error: %v", err)
	}
	ws := WorkspaceManifest(cfg.Name, cfg.Image, cfg.WorkspaceSpec)
	if got := workspaceRepos(ws); !slices.Equal(got, []string{"https://github.com/chalk/chalk", "https://github.com/google/go-cmp"}) {
		t.Fatalf("repos = %v", got)
	}
	if got := workspaceCommands(ws); !slices.Equal(got, []string{"npm ci"}) {
		t.Fatalf("commands = %v", got)
	}
	if !strings.HasPrefix(cfg.Name, "swarm-") || len(cfg.Name) != len("swarm-")+6 {
		t.Fatalf("generated Name = %q, want swarm-<6 hex>", cfg.Name)
	}
}

func TestTimeoutFlagDurationSyntax(t *testing.T) {
	var f timeoutFlag
	if err := f.Set("5m"); err != nil {
		t.Fatalf("Set(5m) error: %v", err)
	}
	if time.Duration(f) != 5*time.Minute {
		t.Fatalf("got %s, want 5m", time.Duration(f))
	}
	if err := f.Set("0"); err == nil {
		t.Fatalf("expected error for 0 timeout")
	}
	if err := f.Set("-5s"); err == nil {
		t.Fatalf("expected error for negative duration")
	}
}

func TestWorkspaceConfig(t *testing.T) {
	specImage := "localhost:5001/ax-runner@sha256:" + strings.Repeat("b", 64)
	defaultImg := "localhost:5001/ax-runner@sha256:" + strings.Repeat("d", 64)
	path := filepath.Join(t.TempDir(), "ws.json")
	spec := `{
	  "profile": {"image": "` + specImage + `", "resources": {"cpu": "2"}},
	  "setup": {
	    "git": [{"repo": "https://github.com/chalk/chalk", "ref": "main"}],
	    "commands": ["npm ci"],
	    "gateway": {"name": "someone-else"}
	  }
	}`
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	parse := func(args ...string) Config {
		t.Helper()
		t.Setenv("AX_TASK_IMAGE", defaultImg)
		cfg, err := parseArgs(append([]string{"--name", "swarm-ws", "--ax-bin", "ax"}, args...), t.TempDir(), &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateConfig(&cfg, t.TempDir()); err != nil {
			t.Fatalf("validateConfig: %v", err)
		}
		return cfg
	}

	// The spec's image wins over the default.
	cfg := parse("--workspace-config", path)
	if cfg.Image != specImage {
		t.Fatalf("Image = %q, want spec image", cfg.Image)
	}
	ws := WorkspaceManifest(cfg.Name, cfg.Image, cfg.WorkspaceSpec)
	if got := workspaceRepos(ws); !slices.Equal(got, []string{"https://github.com/chalk/chalk"}) {
		t.Fatalf("repos = %v", got)
	}
	if got := workspaceCommands(ws); !slices.Equal(got, []string{"npm ci"}) {
		t.Fatalf("commands = %v", got)
	}
	raw, err := json.Marshal(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"swarm-ws"`, `"gateway":{"name":"swarm-ws"}`, `"ref":"main"`, `"resources":{"cpu":"2"}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("manifest missing %s: %s", want, raw)
		}
	}
	// The loaded spec itself is not mutated by building manifests.
	if gw := cfg.WorkspaceSpec["setup"].(map[string]any)["gateway"].(map[string]any); gw["name"] != "someone-else" {
		t.Fatalf("WorkspaceSpec was mutated: %v", gw)
	}
	prompt := BuildSwarmPrompt(cfg, "ax")
	for _, want := range []string{"https://github.com/chalk/chalk", "`npm ci`"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %s", want)
		}
	}

	// Inline JSON and full Workspace manifests are accepted; without
	// profile.image the default image is used.
	cfg = parse("--workspace", `{"kind":"Workspace","metadata":{"name":"ignored"},"spec":{"setup":{"commands":["true"]}}}`)
	if cfg.Image != defaultImg {
		t.Fatalf("Image = %q, want default", cfg.Image)
	}
	ws = WorkspaceManifest(cfg.Name, cfg.Image, cfg.WorkspaceSpec)
	if got := workspaceCommands(ws); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("commands = %v", got)
	}
	if name := ws["metadata"].(map[string]any)["name"]; name != "swarm-ws" {
		t.Fatalf("metadata.name = %v", name)
	}
}

func TestValidateConfigRejections(t *testing.T) {
	validImage := "localhost:5001/ax-runner@sha256:" + strings.Repeat("a", 64)
	base := func() Config {
		return Config{
			Goal:         "test",
			Name:         "swarm-test",
			Atespace:     "default",
			Image:        validImage,
			Parallel:     4,
			SetupTimeout: time.Minute,
			Color:        "auto",
			AXBin:        "ax",
			AgyBin:       "agy",
		}
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"invalid name uppercase", func(c *Config) { c.Name = "Swarm-1" }, "--name"},
		{"name too long", func(c *Config) { c.Name = "a" + strings.Repeat("b", 38) }, "--name"},
		{"invalid atespace", func(c *Config) { c.Atespace = "BAD_SPACE" }, "--atespace"},
		{"flag injection context", func(c *Config) { c.KubeContext = "--evil" }, "--context"},
		{"newline in context", func(c *Config) { c.KubeContext = "ctx\nrm" }, "--context"},
		{"invalid server scheme", func(c *Config) { c.Server = "file:///etc/passwd" }, "--server"},
		{"unpinned image", func(c *Config) { c.Image = "localhost:5001/ax-runner:latest" }, "AX_TASK_IMAGE"},
		{"image with spaces", func(c *Config) { c.Image = validImage + " evil" }, "AX_TASK_IMAGE"},
		{"invalid allow-host", func(c *Config) { c.AllowHosts = []string{"bad host;rm"} }, "--allow-host"},
		{"zero parallel", func(c *Config) { c.Parallel = 0 }, "--parallel"},
		{"invalid effort", func(c *Config) { c.Effort = "ultra" }, "--effort"},
		{"invalid mode", func(c *Config) { c.Mode = "yolo" }, "--mode"},
		{"invalid color", func(c *Config) { c.Color = "rainbow" }, "--color"},
		{"invalid model flag", func(c *Config) { c.Model = "--inject" }, "--model"},
		{"control char in ax bin", func(c *Config) { c.AXBin = "ax\nfoo" }, "--ax-bin"},
		{"malformed allow rule", func(c *Config) { c.AllowRules = []string{"mcp"} }, "--allow"},
		{"unknown allow action", func(c *Config) { c.AllowRules = []string{"shell(ls)"} }, "--allow"},
		{"inline workspace not json", func(c *Config) { c.Workspace = "{nope" }, "--workspace"},
		{"workspace and workspace-config", func(c *Config) { c.Workspace = "{}"; c.WorkspaceConfig = "ws.json" }, "not both"},
		{"workspace config missing file", func(c *Config) { c.WorkspaceConfig = "/nonexistent/ws.json" }, "--workspace-config"},
		{"workspace config wrong kind", func(c *Config) { c.Workspace = `{"kind":"Gateway","spec":{}}` }, "--workspace"},
		{"workspace config bad setup", func(c *Config) { c.Workspace = `{"setup":[]}` }, "--workspace"},
		{"workspace config bad repo", func(c *Config) {
			c.Workspace = `{"setup":{"git":[{"repo":"file:///etc/passwd"}]}}`
		}, "--workspace"},
		{"workspace config unpinned image", func(c *Config) {
			c.Workspace = `{"profile":{"image":"localhost:5001/ax-runner:latest"}}`
		}, "AX_TASK_IMAGE"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			err := validateConfig(&cfg, "/tmp/ax")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestDryRun(t *testing.T) {
	validImage := "localhost:5001/ax-runner@sha256:" + strings.Repeat("a", 64)
	stdoutFile, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	stdinFile, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}

	code := run([]string{
		"--dry-run",
		"--name", "swarm-dry1",
		"--workspace", `{"profile":{"image":"` + validImage + `"},"setup":{"git":[{"repo":"https://github.com/chalk/chalk"}]}}`,
		"find", "weak", "spots",
	}, stdinFile, stdoutFile, stderrFile)
	if code != 0 {
		t.Fatalf("run(--dry-run) exit code = %d, want 0", code)
	}

	outBytes, _ := os.ReadFile(stdoutFile.Name())
	errBytes, _ := os.ReadFile(stderrFile.Name())
	if !strings.HasPrefix(string(outBytes), "/swarm find weak spots\n") {
		t.Fatalf("dry-run stdout did not start with /swarm prompt: %q", string(outBytes))
	}
	if !strings.Contains(string(errBytes), "dry-run: would create Gateway swarm-dry1") {
		t.Fatalf("dry-run stderr missing summary: %q", string(errBytes))
	}
	if !strings.Contains(string(errBytes), `"repo": "https://github.com/chalk/chalk"`) || !strings.Contains(string(errBytes), validImage) {
		t.Fatalf("dry-run stderr missing Workspace manifest: %q", string(errBytes))
	}
}

func TestUIAskAndLifecycle(t *testing.T) {
	var out bytes.Buffer
	ui := newUI(&out, strings.NewReader("audit performance\n"), false, false)
	ui.Start()
	ui.Status("setting up", true)
	ui.Log("hello")
	ans, ok, err := ui.Ask("? ")
	ui.Stop()
	if err != nil || !ok || ans != "audit performance" {
		t.Fatalf("Ask() = (%q, %v, %v), want (audit performance, true, nil)", ans, ok, err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("expected log output in %q", out.String())
	}
}

func TestAgyMissingMessageAndResolve(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.local/bin/agy fallback
	t.Setenv("PATH", t.TempDir())

	if _, err := resolveAgy("agy"); err == nil {
		t.Fatalf("expected agy to be missing")
	}
	msg := agyMissingMessage("agy")
	for _, want := range []string{"not installed", agyInstallCommand, "https://antigravity.google/docs/getting-started"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
	if custom := agyMissingMessage("/opt/agy"); !strings.Contains(custom, `"/opt/agy"`) || !strings.Contains(custom, "--agy-bin") {
		t.Fatalf("custom-path message should name the path and flag:\n%s", custom)
	}

	// The installer's default location is used when agy is not on PATH.
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveAgy("agy"); err != nil || got != filepath.Join(bin, "agy") {
		t.Fatalf("resolveAgy fallback = %q, %v", got, err)
	}
}
