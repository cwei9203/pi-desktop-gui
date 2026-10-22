package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDesktopSessionAndInterruption(t *testing.T) {
	d, err := newDesktop(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.disconnect() })
	project := t.TempDir()
	project, _ = filepath.EvalSymlinks(project)
	snapshot, err := d.Start(project, fakePi(t), false)
	if err != nil || !snapshot.Connected || snapshot.Sequence == 0 {
		t.Fatalf("connect: %+v, %v", snapshot, err)
	}
	if _, err := d.Send("hello", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NewSession(); err == nil {
		t.Fatal("switched an active session")
	}
	queued, err := d.Stop()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(queued)
	if !strings.Contains(string(encoded), "queued instruction") {
		t.Fatalf("queued message lost: %s", encoded)
	}
	snapshot, err = d.Snapshot()
	if err != nil || snapshot.Running || len(snapshot.Messages) != 1 || len(snapshot.Sessions) != 1 {
		t.Fatalf("stopped snapshot: %+v, %v", snapshot, err)
	}
	if snapshot.Sessions[0].Name != "hello" {
		t.Fatalf("session has no useful title: %q", snapshot.Sessions[0].Name)
	}
	path := snapshot.Sessions[0].Path
	if _, err := d.NewSession(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resume(path); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resume(filepath.Join(t.TempDir(), "outside.jsonl")); err == nil {
		t.Fatal("resumed a session outside the GUI project")
	}
	settings := d.GetSettings()
	if settings.LastProject != project || !slices.Contains(settings.Projects, project) {
		t.Fatalf("project was not saved: %+v", settings)
	}
}

func TestDesktopStopsPromptWaitingForExtensionInput(t *testing.T) {
	d, err := newDesktop(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.disconnect() })
	if _, err := d.Start(t.TempDir(), fakePi(t), false); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() { _, err := d.Send("/ask", ""); sent <- err }()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-d.wake:
			d.mu.Lock()
			waiting := len(d.pending) == 1
			d.mu.Unlock()
			if waiting {
				ready := make(chan Snapshot, 1)
				go func() { snapshot, _ := d.Snapshot(); ready <- snapshot }()
				select {
				case snapshot := <-ready:
					if len(snapshot.PendingUI) != 1 {
						t.Fatal("snapshot lost the pending input")
					}
				case <-time.After(time.Second):
					t.Fatal("snapshot blocked behind the pending prompt")
				}
				if _, err := d.Stop(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-sent:
					if err != nil {
						t.Fatal(err)
					}
				case <-deadline:
					t.Fatal("prompt did not unblock after stop")
				}
				return
			}
		case <-deadline:
			t.Fatal("extension input never arrived")
		}
	}
}

func fakePi(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pi")
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\nexec " + quoted + " -test.run=^TestDesktopPiProcess$ -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDesktopPiProcess(t *testing.T) {
	boundary := slices.Index(os.Args, "--")
	if boundary < 0 {
		return
	}
	args := os.Args[boundary+1:]
	if slices.Contains(args, "--version") {
		fmt.Println("1.4.0")
		os.Exit(0)
	}
	directory := args[slices.Index(args, "--session-dir")+1]
	file := filepath.Join(directory, "session.jsonl")
	var messages []any
	var pending map[string]any
	running := false
	emit := func(value any) { data, _ := json.Marshal(value); fmt.Println(string(data)) }
	respond := func(command map[string]any, data any) {
		emit(map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true, "data": data})
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var command map[string]any
		if json.Unmarshal(scanner.Bytes(), &command) != nil {
			os.Exit(2)
		}
		switch command["type"] {
		case "get_state":
			respond(command, map[string]any{"sessionFile": file, "isStreaming": running})
		case "get_messages":
			respond(command, map[string]any{"messages": messages})
		case "get_available_models":
			respond(command, map[string]any{"models": []any{}})
		case "new_session":
			messages = nil
			file = filepath.Join(directory, "new-session.jsonl")
			respond(command, map[string]any{"cancelled": false})
		case "switch_session":
			file, _ = command["sessionPath"].(string)
			respond(command, map[string]any{"cancelled": false})
		case "prompt":
			if command["message"] == "/ask" {
				pending = command
				emit(map[string]any{"type": "extension_ui_request", "id": "ask-1", "method": "confirm", "title": "Continue?"})
				continue
			}
			running = true
			messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": command["message"]}}})
			_ = os.WriteFile(file, []byte("{}\n"), 0600)
			emit(map[string]any{"type": "agent_start"})
			respond(command, map[string]any{"disposition": "started"})
		case "extension_ui_response":
			if pending != nil {
				respond(pending, map[string]any{"disposition": "handled"})
				pending = nil
			}
		case "clear_queue":
			respond(command, map[string]any{"steering": []string{"queued instruction"}, "followUp": []string{}})
		case "abort":
			running = false
			emit(map[string]any{"type": "agent_settled", "aborted": true})
			respond(command, nil)
		}
	}
	os.Exit(0)
}

func TestDesktopRealPiConnection(t *testing.T) {
	executable := os.Getenv("PI_GUI_SMOKE_PI")
	if executable == "" {
		t.Skip("set PI_GUI_SMOKE_PI to test the installed Pi without a model call")
	}
	d, err := newDesktop(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.disconnect() })
	d.data.PiPath = executable
	installation := d.DetectPi()
	if installation.Status != "ready" || installation.Path != executable {
		t.Fatalf("Pi auto-detection failed: %+v", installation)
	}
	snapshot, err := d.Start(t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Connected || snapshot.State["sessionFile"] == nil {
		t.Fatal("Pi did not return a persistent session")
	}
	t.Logf("Pi %s auto-detected and RPC connected; %d models available", installation.Version, len(snapshot.Models))
}

func TestDesktopTransientSnapshotBoundary(t *testing.T) {
	d, err := newDesktop(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	emit := func(value string) { d.receive(0, json.RawMessage(value)) }
	emit(`{"type":"message_start","message":{"role":"assistant","content":[]}}`)
	emit(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hello"}}`)
	emit(`{"type":"tool_execution_start","toolCallId":"read-1","toolName":"read"}`)
	emit(`{"type":"tool_execution_update","toolCallId":"read-1","toolName":"read","partialResult":"reading"}`)
	emit(`{"type":"queue_update","steering":[],"followUp":["next"]}`)
	emit(`{"type":"response","command":"get_messages","success":true,"data":{"messages":[]}}`)
	saved := d.snapshot
	if len(saved.Transient) != 4 || saved.Sequence != 6 {
		t.Fatalf("incomplete snapshot: %+v", saved)
	}
	emit(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":" world"}}`)
	if len(saved.Transient) != 4 || saved.Sequence != 6 {
		t.Fatal("snapshot changed after its sequence boundary")
	}
	emit(`{"type":"message_end","message":{"role":"assistant","content":[]}}`)
	emit(`{"type":"tool_execution_end","toolCallId":"read-1"}`)
	emit(`{"type":"agent_settled"}`)
	emit(`{"type":"response","command":"get_messages","success":true,"data":{"messages":[]}}`)
	if len(d.snapshot.Transient) != 1 || d.snapshot.Transient[0].(map[string]any)["type"] != "queue_update" {
		t.Fatal("completed work leaked into the snapshot or queued work was lost")
	}
	d.rememberTransient(map[string]any{"type": "message_update"}, 16*1024*1024+1)
	if !d.transientOverflow || len(d.transient) != 0 {
		t.Fatal("transient buffer was not bounded")
	}
	emit(`{"type":"agent_settled"}`)
	if d.transientOverflow {
		t.Fatal("completed response did not restore snapshot availability")
	}
}

func TestDesktopQuitRejectsNewWork(t *testing.T) {
	d, err := newDesktop(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d.quitting = true
	if _, err := d.Send("hello", ""); err == nil {
		t.Fatal("sent while quitting")
	}
	if _, err := d.NewSession(); err == nil {
		t.Fatal("created session while quitting")
	}
}
