package pi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cwei9203/pi-desktop-gui/internal/pi"
)

func startFixture(t *testing.T, emit func(json.RawMessage)) *pi.Client {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client, err := pi.Start(executable, []string{"-test.run=TestPiFixture", "--", "pi-fixture"}, t.TempDir(), emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCallsCorrelateOutOfOrderResponsesAndPreserveLargeUnicodeEvents(t *testing.T) {
	events := make(chan json.RawMessage, 4)
	client := startFixture(t, func(event json.RawMessage) { events <- event })
	ctx := testContext(t)
	first := make(chan json.RawMessage, 1)
	errors := make(chan error, 1)
	go func() {
		data, err := client.Call(ctx, "hold", nil)
		first <- data
		errors <- err
	}()
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	data, err := client.Call(ctx, "release", nil)
	if err != nil || string(data) != `{"answer":"second"}` {
		t.Fatalf("second call: %s, %v", data, err)
	}
	if data = <-first; string(data) != `{"answer":"first"}` {
		t.Fatalf("first call: %s", data)
	}
	if err = <-errors; err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"second", "first"} {
		select {
		case raw := <-events:
			var response struct {
				Type string
				Data struct{ Answer string }
			}
			if err := json.Unmarshal(raw, &response); err != nil || response.Type != "response" || response.Data.Answer != expected {
				t.Fatalf("missing ordered response event %q", expected)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	select {
	case raw := <-events:
		var event struct{ Text string }
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		if event.Text != strings.Repeat("中文", 20000)+"\u2028\u2029" {
			t.Fatalf("large Unicode event changed: %d bytes", len(event.Text))
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestStopReturnsQueueAndPreventsItFromContinuing(t *testing.T) {
	client := startFixture(t, nil)
	data, err := client.Stop(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"steering":[],"followUp":["finish later"]}` {
		t.Fatalf("unexecuted queue was not returned: %s", data)
	}
}

func TestCancelledCallIsNotResentAndLateResponseIsIgnored(t *testing.T) {
	client := startFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := client.Call(ctx, "silent", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	data, err := client.Call(testContext(t), "get_received", nil)
	if err != nil || string(data) != `{"count":1}` {
		t.Fatalf("cancelled call was resent or consumed the next reply: %s, %v", data, err)
	}
}

func TestCommandFailureDoesNotBreakConnection(t *testing.T) {
	client := startFixture(t, nil)
	ctx := testContext(t)
	if _, err := client.Call(ctx, "unknown", nil); err == nil || err.Error() != "unknown command" {
		t.Fatalf("expected command failure, got %v", err)
	}
	data, err := client.Call(ctx, "ping", nil)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("connection did not recover: %s, %v", data, err)
	}
}

func TestProtocolFailureOrExitRejectsEveryPendingCall(t *testing.T) {
	for _, command := range []string{"malformed", "exit", "unterminated", "parse_failure", "stdout_closed"} {
		t.Run(command, func(t *testing.T) {
			events := make(chan json.RawMessage, 4)
			client := startFixture(t, func(event json.RawMessage) { events <- event })
			ctx := testContext(t)
			first := make(chan error, 1)
			go func() {
				_, err := client.Call(ctx, "hold", nil)
				first <- err
			}()
			select {
			case <-events:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if _, err := client.Call(ctx, command, nil); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("failed process did not reject the command: %v", err)
			}
			if err := <-first; err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("failed process left an earlier call waiting: %v", err)
			}
			select {
			case <-client.Done():
				if client.Err() == nil {
					t.Fatal("missing process failure")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestExtensionReplyResumesTheWaitingRequest(t *testing.T) {
	events := make(chan json.RawMessage, 4)
	client := startFixture(t, func(event json.RawMessage) { events <- event })
	ctx := testContext(t)
	finished := make(chan error, 1)
	go func() {
		data, err := client.Call(ctx, "request_input", nil)
		if err == nil && string(data) != `{"value":"继续"}` {
			err = fmt.Errorf("unexpected extension value: %s", data)
		}
		finished <- err
	}()
	select {
	case raw := <-events:
		var request struct{ Type, ID string }
		if err := json.Unmarshal(raw, &request); err != nil || request.Type != "extension_ui_request" {
			t.Fatalf("expected extension request: %s, %v", raw, err)
		}
		if err := client.Reply(ctx, request.ID, map[string]any{"value": "继续"}); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsCannotBlockProtocolResponses(t *testing.T) {
	client := startFixture(t, nil)
	data, err := client.Call(testContext(t), "diagnostics", nil)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("stderr blocked stdout: %s, %v", data, err)
	}
}

func TestBrokenInputRejectsEarlierPendingCalls(t *testing.T) {
	held := make(chan struct{}, 1)
	client := startFixture(t, func(raw json.RawMessage) {
		var event struct{ Type string }
		_ = json.Unmarshal(raw, &event)
		if event.Type == "held" {
			held <- struct{}{}
		}
	})
	ctx := testContext(t)
	finished := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "hold", nil)
		finished <- err
	}()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := client.Call(ctx, "stdin_closed", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, "ping", nil); err == nil {
		t.Fatal("write to a closed pipe succeeded")
	}
	if err := <-finished; err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("broken pipe did not reject an earlier request: %v", err)
	}
	select {
	case <-client.Done():
	case <-ctx.Done():
		t.Fatal("broken connection left Pi running")
	}
}

func TestCloseRejectsPendingCallsAndReapsToolProcesses(t *testing.T) {
	held := make(chan struct{}, 1)
	client := startFixture(t, func(raw json.RawMessage) {
		var event struct{ Type string }
		_ = json.Unmarshal(raw, &event)
		if event.Type == "held" {
			held <- struct{}{}
		}
	})
	ctx := testContext(t)
	data, err := client.Call(ctx, "spawn_child", nil)
	if err != nil {
		t.Fatal(err)
	}
	var child struct{ PID int }
	if err = json.Unmarshal(data, &child); err != nil || child.PID <= 0 {
		t.Fatalf("missing tool process: %s, %v", data, err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "hold", nil)
		finished <- err
	}()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closed := make(chan error, 2)
	for range 2 {
		go func() { closed <- client.Close() }()
	}
	for range 2 {
		if err = <-closed; err != nil {
			t.Fatal(err)
		}
	}
	if err = <-finished; !errors.Is(err, pi.ErrClosed) {
		t.Fatalf("pending call survived close: %v", err)
	}
	for syscall.Kill(child.PID, 0) == nil {
		select {
		case <-ctx.Done():
			t.Fatalf("tool process %d outlived Pi", child.PID)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestCancelledBlockedWriteTerminatesTheBrokenConnection(t *testing.T) {
	client := startFixture(t, nil)
	if _, err := client.Call(testContext(t), "stop_reading", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := client.Call(ctx, "prompt", map[string]any{"message": strings.Repeat("x", 1<<20)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked write did not cancel: %v", err)
	}
	select {
	case <-client.Done():
		if client.Err() == nil {
			t.Fatal("partial record must not leave the connection reusable")
		}
	case <-testContext(t).Done():
		t.Fatal("blocked writer left Pi running")
	}
}

func TestCloseTerminatesUnresponsivePi(t *testing.T) {
	client := startFixture(t, nil)
	if _, err := client.Call(testContext(t), "stop_reading", nil); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := client.Close(); err == nil {
		t.Fatal("forced termination must report that orderly shutdown failed")
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("closing an unresponsive process exceeded its deadline")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("Pi was not reaped")
	}
}

func TestPiFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] == "pi-grandchild" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if os.Args[len(os.Args)-1] != "pi-fixture" {
		return
	}
	var held string
	queued := true
	received := 0
	reader := bufio.NewScanner(os.Stdin)
	for reader.Scan() {
		var command map[string]any
		if err := json.Unmarshal(reader.Bytes(), &command); err != nil {
			os.Exit(2)
		}
		id, _ := command["id"].(string)
		switch command["type"] {
		case "spawn_child":
			executable, _ := os.Executable()
			child := exec.Command(executable, "-test.run=TestPiFixture", "--", "pi-grandchild")
			if err := child.Start(); err != nil {
				os.Exit(9)
			}
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"pid\":%d}}\n", id, child.Process.Pid)
		case "diagnostics":
			fmt.Fprint(os.Stderr, strings.Repeat("diagnostic\n", 32768))
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"ok\":true}}\n", id)
		case "stop_reading":
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true}\n", id)
			time.Sleep(time.Hour)
		case "ping":
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"ok\":true}}\n", id)
		case "silent":
			held = id
			received++
		case "get_received":
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"late\":true}}\n", held)
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"count\":%d}}\n", id, received)
		case "malformed":
			fmt.Println("not JSON")
		case "stdout_closed":
			_ = os.Stdout.Close()
			time.Sleep(time.Hour)
		case "stdin_closed":
			_ = os.Stdin.Close()
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true}\n", id)
			time.Sleep(time.Hour)
		case "parse_failure":
			fmt.Println(`{"type":"response","command":"parse","success":false,"error":"Failed to parse command"}`)
		case "exit":
			os.Exit(7)
		case "unterminated":
			fmt.Print(`{"type":"event"}`)
			os.Exit(0)
		case "request_input":
			held = id
			fmt.Println(`{"type":"extension_ui_request","id":"input-1","method":"input","title":"Choose"}`)
		case "extension_ui_response":
			if id != "input-1" {
				os.Exit(8)
			}
			value, _ := json.Marshal(command["value"])
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"value\":%s}}\n", held, value)
		case "hold":
			held = id
			fmt.Println(`{"type":"held"}`)
		case "release":
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"answer\":\"second\"}}\r\n", id)
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"answer\":\"first\"}}\n", held)
			text := strings.Repeat("中文", 20000) + "\u2028\u2029"
			raw, _ := json.Marshal(map[string]any{"type": "large", "text": text})
			// Write byte by byte at the start so UTF-8 characters cross read boundaries.
			for _, b := range raw[:100] {
				_, _ = os.Stdout.Write([]byte{b})
			}
			_, _ = os.Stdout.Write(append(raw[100:], '\n'))
		case "clear_queue":
			queued = false
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true,\"data\":{\"steering\":[],\"followUp\":[\"finish later\"]}}\n", id)
		case "abort":
			if queued {
				fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":false,\"error\":\"queued task continued\"}\n", id)
			} else {
				fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":true}\n", id)
			}
		default:
			fmt.Fprintf(os.Stdout, "{\"type\":\"response\",\"id\":%q,\"success\":false,\"error\":\"unknown command\"}\n", id)
		}
	}
	if err := reader.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		os.Exit(3)
	}
	os.Exit(0)
}
