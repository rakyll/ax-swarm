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
	"slices"
	"strings"
	"testing"
)

func TestBuildAgyArgs(t *testing.T) {
	minimal := BuildAgyArgs(Config{}, "/swarm hello")
	wantMinimal := []string{"-i", "/swarm hello"}
	if !slices.Equal(minimal, wantMinimal) {
		t.Fatalf("minimal args = %v, want %v", minimal, wantMinimal)
	}

	full := BuildAgyArgs(Config{
		Model:                      "gemini-4-neon-high",
		Effort:                     "high",
		Mode:                       "auto",
		Project:                    "proj-1",
		DangerouslySkipPermissions: true,
	}, "/swarm test")
	wantFull := []string{
		"--model", "gemini-4-neon-high",
		"--effort", "high",
		"--mode", "auto",
		"--project", "proj-1",
		"--dangerously-skip-permissions",
		"-i", "/swarm test",
	}
	if !slices.Equal(full, wantFull) {
		t.Fatalf("full args = %v, want %v", full, wantFull)
	}
}

func TestRunInteractiveAgy(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := RunInteractiveAgy("/bin/echo", []string{"-n", "hello-agy"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunInteractiveAgy() unexpected error: %v", err)
	}
	if stdout.String() != "hello-agy" {
		t.Fatalf("stdout = %q, want hello-agy", stdout.String())
	}
}
