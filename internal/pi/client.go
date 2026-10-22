package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// shortcut: individual RPC records are capped at 32 MiB; revisit for larger attachments.
const maxRecordBytes = 32 << 20

var ErrClosed = errors.New("Pi connection is closed")

type result struct {
	data json.RawMessage
	err  error
}

type Client struct {
	command   *exec.Cmd
	stdin     *os.File
	writeGate chan struct{}
	done      chan struct{}
	emit      func(json.RawMessage)
	mu        sync.Mutex
	pending   map[string]chan result
	sequence  uint64
	err       error
	closing   bool
	closeOnce sync.Once
	closeErr  error
}

// Start owns a child process group; emit must return promptly and may be nil.
func Start(executable string, args []string, directory string, emit func(json.RawMessage)) (*Client, error) {
	command := exec.Command(executable, args...)
	command.Dir = directory
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, output, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr, diagnostics, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, err
	}
	command.Stdout, command.Stderr = output, diagnostics
	if err = command.Start(); err != nil {
		for _, pipe := range []io.Closer{stdin, stdout, output, stderr, diagnostics} {
			_ = pipe.Close()
		}
		return nil, fmt.Errorf("start Pi: %w", err)
	}
	_ = output.Close()
	_ = diagnostics.Close()
	c := &Client{
		command: command, stdin: stdin.(*os.File), emit: emit,
		writeGate: make(chan struct{}, 1), done: make(chan struct{}),
		pending: make(map[string]chan result),
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer stdout.Close()
		err := c.read(stdout)
		if err == nil {
			err = io.EOF
		}
		c.mu.Lock()
		closing := c.closing
		c.mu.Unlock()
		if !closing || (!errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed)) {
			c.fail(err)
			c.kill()
		}
	}()
	go func() {
		defer stderr.Close()
		_, _ = io.Copy(io.Discard, stderr)
	}()
	go func() {
		waitErr := command.Wait()
		// A tool must not outlive the Pi process that owns its process group.
		c.kill()
		select {
		case <-readDone:
		case <-time.After(time.Second):
			_ = stdout.Close()
		}
		_ = stderr.Close()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err == nil && waitErr != nil {
			c.err = fmt.Errorf("Pi process exited: %w", waitErr)
		}
		if c.err == nil && !c.closing {
			c.err = io.EOF
		}
		c.rejectPending(c.connectionError())
		close(c.done)
	}()
	return c, nil
}

func (c *Client) Call(ctx context.Context, kind string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closing || c.err != nil {
		err := c.connectionError()
		c.mu.Unlock()
		return nil, err
	}
	c.sequence++
	id := strconv.FormatUint(c.sequence, 10)
	response := make(chan result, 1)
	c.pending[id] = response
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.write(ctx, kind, id, params); err != nil {
		return nil, err
	}
	select {
	case response := <-response:
		return response.data, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) Reply(ctx context.Context, id string, params map[string]any) error {
	if id == "" {
		return errors.New("extension response requires a request ID")
	}
	return c.write(ctx, "extension_ui_response", id, params)
}

func (c *Client) Stop(ctx context.Context) (json.RawMessage, error) {
	queued, err := c.Call(ctx, "clear_queue", nil)
	if err != nil {
		return nil, fmt.Errorf("clear Pi queue: %w", err)
	}
	_, err = c.Call(ctx, "abort", nil)
	return queued, err
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.rejectPending(ErrClosed)
		c.mu.Unlock()
		_ = c.stdin.Close()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			c.fail(errors.New("Pi did not exit after stdin closed; its process group was terminated"))
			c.kill()
			<-c.done
		}
		c.closeErr = c.Err()
	})
	return c.closeErr
}

func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) write(ctx context.Context, kind, id string, params map[string]any) error {
	record := make(map[string]any, len(params)+2)
	for key, value := range params {
		record[key] = value
	}
	record["id"], record["type"] = id, kind
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) >= maxRecordBytes {
		return errors.New("Pi command exceeds the 32 MiB record limit")
	}
	data = append(data, '\n')
	select {
	case c.writeGate <- struct{}{}:
		defer func() { <-c.writeGate }()
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
	c.mu.Lock()
	closed := c.closing || c.err != nil
	connectionErr := c.connectionError()
	c.mu.Unlock()
	if closed {
		return connectionErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	if err = c.stdin.SetWriteDeadline(deadline); err != nil {
		return err
	}
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = c.stdin.SetWriteDeadline(time.Now())
		close(cancelled)
	})
	n, err := c.stdin.Write(data)
	if !stop() {
		<-cancelled
	}
	_ = c.stdin.SetWriteDeadline(time.Time{})
	if n != len(data) && err == nil {
		err = io.ErrShortWrite
	}
	c.mu.Lock()
	closing := c.closing
	c.mu.Unlock()
	if !closing {
		if n > 0 && n < len(data) {
			c.fail(errors.New("Pi command was only partially written; connection cannot be reused"))
			c.kill()
		} else if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			c.fail(fmt.Errorf("write Pi command: %w", err))
			c.kill()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The pipe deadline can fire before the context timer's goroutine runs.
	if ok && errors.Is(err, os.ErrDeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}

func (c *Client) read(stdout io.Reader) error {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxRecordBytes)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, bytes.TrimSuffix(data[:i], []byte{'\r'}), nil
		}
		if atEOF && len(data) != 0 {
			return 0, nil, errors.New("Pi output ended inside a JSONL record")
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		raw := scanner.Bytes()
		var record struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Success *bool           `json:"success"`
			Data    json.RawMessage `json:"data"`
			Error   string          `json:"error"`
		}
		if !utf8.Valid(raw) || json.Unmarshal(raw, &record) != nil || record.Type == "" {
			return errors.New("invalid Pi JSONL record")
		}
		if record.Type == "response" && record.Success == nil {
			return errors.New("Pi response has no success field")
		}
		if c.emit != nil {
			c.emit(append(json.RawMessage(nil), raw...))
		}
		if record.Type == "response" && record.ID == "" && !*record.Success {
			return fmt.Errorf("Pi rejected an uncorrelated command: %s", record.Error)
		}
		if record.Type == "response" && record.ID != "" {
			response := result{data: record.Data}
			if !*record.Success {
				if record.Error == "" {
					record.Error = "Pi command failed"
				}
				response.err = errors.New(record.Error)
			}
			c.mu.Lock()
			if receiver, ok := c.pending[record.ID]; ok {
				delete(c.pending, record.ID)
				receiver <- response
			}
			c.mu.Unlock()
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Pi output (32 MiB record limit): %w", err)
	}
	return nil
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
	c.rejectPending(c.err)
}

func (c *Client) rejectPending(err error) {
	for id, response := range c.pending {
		delete(c.pending, id)
		response <- result{err: err}
	}
}

func (c *Client) connectionError() error {
	if c.err != nil {
		return c.err
	}
	return ErrClosed
}

// shortcut: cleanup reaches macOS/Linux process-group members; track detached tools before supporting them.
func (c *Client) kill() { _ = syscall.Kill(-c.command.Process.Pid, syscall.SIGKILL) }
