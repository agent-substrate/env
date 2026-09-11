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
	"strings"
	"testing"
)

func TestCommandRouting(t *testing.T) {
	t.Run("root create command", func(t *testing.T) {
		args := []string{"create", "dev1"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "create" {
			t.Errorf("expected command 'create', got %q", cmd.Name())
		}
		if len(remaining) != 1 || remaining[0] != "dev1" {
			t.Errorf("expected remaining args [dev1], got %v", remaining)
		}
	})

	t.Run("root manifest command", func(t *testing.T) {
		args := []string{"manifest"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "manifest" {
			t.Errorf("expected command 'manifest', got %q", cmd.Name())
		}
		if len(remaining) != 0 {
			t.Errorf("expected 0 remaining args, got %v", remaining)
		}
	})

	t.Run("top-level suspend command", func(t *testing.T) {
		args := []string{"suspend", "dev1"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "suspend" {
			t.Errorf("expected command 'suspend', got %q", cmd.Name())
		}
		if len(remaining) != 1 || remaining[0] != "dev1" {
			t.Errorf("expected 1 remaining arg [dev1], got %v", remaining)
		}
	})

	t.Run("top-level delete command", func(t *testing.T) {
		args := []string{"delete", "dev1"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "delete" {
			t.Errorf("expected command 'delete', got %q", cmd.Name())
		}
		if len(remaining) != 1 || remaining[0] != "dev1" {
			t.Errorf("expected 1 remaining arg [dev1], got %v", remaining)
		}
	})

	t.Run("env read routes to env subcommand", func(t *testing.T) {
		args := []string{"dev1", "read", "/note.txt"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "read" {
			t.Errorf("expected cmd 'read', got %q", cmd.Name())
		}
		if len(remaining) != 1 || remaining[0] != "/note.txt" {
			t.Errorf("expected remaining args [/note.txt], got %v", remaining)
		}
	})

	t.Run("env write routes to env subcommand", func(t *testing.T) {
		args := []string{"dev1", "write", "/note.txt"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "write" {
			t.Errorf("expected cmd 'write', got %q", cmd.Name())
		}
		if len(remaining) != 1 || remaining[0] != "/note.txt" {
			t.Errorf("expected remaining args [/note.txt], got %v", remaining)
		}
	})

	t.Run("env shell routes to env subcommand", func(t *testing.T) {
		args := []string{"dev1", "shell", "uname", "-a"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "shell" {
			t.Errorf("expected cmd 'shell', got %q", cmd.Name())
		}
		if len(remaining) != 2 || remaining[0] != "uname" || remaining[1] != "-a" {
			t.Errorf("expected remaining args [uname -a], got %v", remaining)
		}
	})

	t.Run("env cmd alias routes to shell", func(t *testing.T) {
		args := []string{"dev1", "cmd", "uname", "-a"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "shell" {
			t.Errorf("expected cmd 'shell' for alias 'cmd', got %q", cmd.Name())
		}
		if len(remaining) != 2 {
			t.Errorf("expected 2 remaining args, got %v", remaining)
		}
	})

	t.Run("env with flags parses flags and routes to subcommand", func(t *testing.T) {
		args := []string{"dev1", "--api", "127.0.0.1:7777", "read", "/note.txt"}
		root := newRootCommand(args)
		cmd, remaining, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find failed: %v", err)
		}
		if cmd.Name() != "read" {
			t.Errorf("expected cmd 'read', got %q", cmd.Name())
		}
		if err := cmd.ParseFlags(remaining); err != nil {
			t.Fatalf("cmd.ParseFlags failed: %v", err)
		}
		parsedArgs := cmd.Flags().Args()
		if len(parsedArgs) != 1 || parsedArgs[0] != "/note.txt" {
			t.Errorf("expected parsed args [/note.txt], got %v", parsedArgs)
		}
	})
}

func TestHelpOutput(t *testing.T) {
	t.Run("root help", func(t *testing.T) {
		root := newRootCommand(nil)

		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetArgs([]string{"--help"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"create", "manifest", "suspend", "delete", "Common environment commands", "ate-env <id> read <path>"} {
			if !bytes.Contains(buf.Bytes(), []byte(want)) {
				t.Errorf("help output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("env help", func(t *testing.T) {
		args := []string{"dev1", "--help"}
		root := newRootCommand(args)

		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"read", "write", "shell"} {
			if !bytes.Contains(buf.Bytes(), []byte(want)) {
				t.Errorf("env help output missing %q:\n%s", want, out)
			}
		}
		for _, dontWant := range []string{"suspend", "delete"} {
			if strings.Contains(out, "Available Commands:\n") && strings.Contains(out, dontWant) {
				t.Errorf("env help should not list %q:\n%s", dontWant, out)
			}
		}
	})
}
