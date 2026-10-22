package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cwei9203/pi-desktop-gui/internal/localstate"
	"github.com/cwei9203/pi-desktop-gui/internal/pi"
	"github.com/egoist/mygo"
)

type PiRecord struct {
	Generation int    `json:"generation"`
	Sequence   int    `json:"sequence"`
	Record     string `json:"record"`
}

var Record = mygo.NewEvent[PiRecord]("pi:record")

type Settings struct {
	PiPath      string   `json:"piPath"`
	Projects    []string `json:"projects"`
	LastProject string   `json:"lastProject"`
}

type Snapshot struct {
	Generation int                  `json:"generation"`
	Sequence   int                  `json:"sequence"`
	Project    string               `json:"project"`
	Connected  bool                 `json:"connected"`
	Running    bool                 `json:"running"`
	State      map[string]any       `json:"state"`
	Messages   []any                `json:"messages"`
	Models     []any                `json:"models"`
	Sessions   []localstate.Session `json:"sessions"`
	PendingUI  []any                `json:"pendingUI"`
	Transient  []any                `json:"transient"`
}

type transientRecord struct {
	record map[string]any
	bytes  int
}

type Desktop struct {
	op                sync.Mutex
	mu                sync.Mutex
	dir               string
	data              localstate.Data
	client            *pi.Client
	project           string
	generation        int
	sequence          int
	running           bool
	stopping          bool
	submitting        bool
	quitting          bool
	pending           map[string]map[string]any
	snapshot          Snapshot
	events            []PiRecord
	eventBytes        int
	wake              chan struct{}
	transient         []transientRecord
	transientOverflow bool
}

func newDesktop(dir string) (*Desktop, error) {
	data, err := localstate.Load(filepath.Join(dir, "settings.json"))
	if err != nil {
		return nil, err
	}
	return &Desktop{dir: dir, data: data, pending: make(map[string]map[string]any), wake: make(chan struct{}, 1)}, nil
}

func (d *Desktop) GetSettings() Settings {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Settings{d.data.PiPath, slices.Clone(d.data.Projects), d.data.LastProject}
}

func (d *Desktop) ChooseProject(ctx context.Context) (string, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: mygo.CallerWindow(ctx), Title: "打开项目", Directory: true})
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[0], nil
}

func (d *Desktop) OpenExternal(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return errors.New("仅支持打开 HTTP 或 HTTPS 网页")
	}
	return mygo.Shell.OpenExternal(raw)
}

func (d *Desktop) Start(project, executable string, approve bool) (Snapshot, error) {
	d.op.Lock()
	defer d.op.Unlock()
	if err := d.checkQuitting(); err != nil {
		return Snapshot{}, err
	}
	if d.busy() {
		return Snapshot{}, errors.New("请先停止当前任务，再切换项目")
	}
	project, err := filepath.Abs(project)
	if err != nil {
		return Snapshot{}, err
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return Snapshot{}, fmt.Errorf("项目目录不可访问: %w", err)
	}
	if info, err := os.Stat(project); err != nil || !info.IsDir() {
		return Snapshot{}, errors.New("请选择有效的项目目录")
	}
	var installation PiInstallation
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if strings.TrimSpace(executable) == "" {
		installation = d.DetectPi()
	} else {
		installation = probePi(ctx, executable)
	}
	if installation.Status != "ready" {
		return Snapshot{}, errors.New(installation.Detail)
	}
	executable = installation.Path
	sessionDir := d.sessionDir(project)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return Snapshot{}, err
	}
	args := []string{"--mode", "rpc", "--session-dir", sessionDir}
	if approve {
		args = append(args, "--approve")
	} else {
		args = append(args, "--no-approve")
	}
	d.mu.Lock()
	for _, session := range d.data.Sessions {
		if session.Project == project && d.ownedSession(project, session.Path) {
			args = append(args, "--session", session.Path)
			break
		}
	}
	d.mu.Unlock()
	if err := d.disconnect(); err != nil {
		return Snapshot{}, err
	}
	d.mu.Lock()
	d.project = project
	d.generation++
	generation := d.generation
	d.sequence, d.running, d.stopping = 0, false, false
	d.pending = make(map[string]map[string]any)
	d.snapshot = Snapshot{Generation: generation, Project: project}
	d.transient, d.transientOverflow = nil, false
	d.mu.Unlock()
	client, err := pi.Start(executable, args, project, func(record json.RawMessage) { d.receive(generation, record) })
	if err != nil {
		return Snapshot{}, err
	}
	d.mu.Lock()
	d.client = client
	d.data.PiPath, d.data.LastProject = executable, project
	d.data.Projects = append([]string{project}, slices.DeleteFunc(slices.Clone(d.data.Projects), func(p string) bool { return p == project })...)
	if len(d.data.Projects) > 20 {
		d.data.Projects = d.data.Projects[:20]
	}
	d.mu.Unlock()
	go func() {
		<-client.Done()
		message := "Pi 已断开连接"
		if err := client.Err(); err != nil {
			message = err.Error()
		}
		record, _ := json.Marshal(map[string]any{"type": "desktop_disconnected", "error": message})
		d.receive(generation, record)
	}()
	snapshot, err := d.refresh()
	if err != nil {
		_ = d.disconnect()
		return Snapshot{}, fmt.Errorf("Pi 初始化失败: %w", err)
	}
	return snapshot, nil
}

func (d *Desktop) Snapshot() (Snapshot, error) {
	d.op.Lock()
	defer d.op.Unlock()
	return d.refresh()
}

func (d *Desktop) NewSession() (Snapshot, error) {
	d.op.Lock()
	defer d.op.Unlock()
	if err := d.checkQuitting(); err != nil {
		return Snapshot{}, err
	}
	return d.switchSession("new_session", nil)
}

func (d *Desktop) Resume(path string) (Snapshot, error) {
	d.op.Lock()
	defer d.op.Unlock()
	if err := d.checkQuitting(); err != nil {
		return Snapshot{}, err
	}
	d.mu.Lock()
	project := d.project
	known := slices.ContainsFunc(d.data.Sessions, func(s localstate.Session) bool { return s.Project == project && s.Path == path })
	d.mu.Unlock()
	if !known || !d.ownedSession(project, path) {
		return Snapshot{}, errors.New("该会话不属于当前项目，或尚未保存到磁盘")
	}
	return d.switchSession("switch_session", map[string]any{"sessionPath": path})
}

func (d *Desktop) switchSession(command string, params map[string]any) (Snapshot, error) {
	if d.busy() {
		return Snapshot{}, errors.New("请先停止当前任务，再切换会话")
	}
	result, err := d.call(command, params)
	if err != nil {
		return Snapshot{}, err
	}
	var response struct {
		Cancelled bool `json:"cancelled"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return Snapshot{}, err
	}
	if response.Cancelled {
		return Snapshot{}, errors.New("扩展取消了会话切换，当前会话保持不变")
	}
	return d.refresh()
}

func (d *Desktop) Send(message, behavior string) (any, error) {
	if strings.TrimSpace(message) == "" || len(message) > 1024*1024 {
		return nil, errors.New("消息不能为空，且不能超过 1 MiB")
	}
	if behavior != "" && behavior != "steer" && behavior != "followUp" {
		return nil, errors.New("不支持的发送方式")
	}
	d.op.Lock()
	d.mu.Lock()
	if d.stopping || d.submitting || d.quitting {
		d.mu.Unlock()
		d.op.Unlock()
		return nil, errors.New("正在提交、停止或退出，请稍后发送")
	}
	client := d.client
	d.submitting = true
	d.mu.Unlock()
	d.op.Unlock()
	defer func() { d.mu.Lock(); d.submitting = false; d.mu.Unlock() }()
	if client == nil {
		return nil, errors.New("Pi 未连接，请先打开项目")
	}
	params := map[string]any{"message": message}
	if behavior != "" {
		params["streamingBehavior"] = behavior
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := client.Call(ctx, "prompt", params)
	if err != nil {
		return nil, err
	}
	var data any
	err = json.Unmarshal(result, &data)
	return data, err
}

func (d *Desktop) Stop() (any, error) {
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		return nil, errors.New("正在停止")
	}
	d.stopping = true
	client := d.client
	ids := make([]string, 0, len(d.pending))
	for id := range d.pending {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.stopping = false; d.mu.Unlock() }()
	if client == nil {
		return nil, errors.New("Pi 未连接")
	}
	for _, id := range ids {
		if err := d.Respond(id, map[string]any{"cancelled": true}); err != nil && !errors.Is(err, errInputExpired) {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := client.Stop(ctx)
	var data any
	if len(result) > 0 {
		_ = json.Unmarshal(result, &data)
	}
	if err != nil {
		// Preserve cleared messages even when the subsequent abort fails.
		return map[string]any{"queue": data, "stopError": err.Error()}, nil
	}
	return data, nil
}

var errInputExpired = errors.New("该输入请求已失效")

func (d *Desktop) Respond(id string, response map[string]any) error {
	d.mu.Lock()
	request, ok := d.pending[id]
	client := d.client
	generation := d.generation
	d.mu.Unlock()
	if !ok || client == nil {
		return errInputExpired
	}
	params := make(map[string]any)
	if cancelled, ok := response["cancelled"].(bool); ok && cancelled {
		params["cancelled"] = true
	} else if request["method"] == "confirm" {
		confirmed, ok := response["confirmed"].(bool)
		if !ok {
			return errors.New("确认结果必须为布尔值")
		}
		params["confirmed"] = confirmed
	} else {
		value, ok := response["value"].(string)
		if !ok || len(value) > 1024*1024 {
			return errors.New("请输入有效文字，最多 1 MiB")
		}
		if request["method"] == "select" {
			options, _ := request["options"].([]any)
			if !slices.Contains(options, any(value)) {
				return errors.New("选项已失效")
			}
		}
		params["value"] = value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Reply(ctx, id, params); err != nil {
		return err
	}
	resolved, _ := json.Marshal(map[string]any{"type": "extension_ui_resolved", "id": id})
	d.receive(generation, resolved)
	return nil
}

func (d *Desktop) SetModel(provider, id string) (Snapshot, error) {
	d.op.Lock()
	defer d.op.Unlock()
	if err := d.checkQuitting(); err != nil {
		return Snapshot{}, err
	}
	if d.busy() {
		return Snapshot{}, errors.New("执行期间不能切换模型")
	}
	if _, err := d.call("set_model", map[string]any{"provider": provider, "modelId": id}); err != nil {
		return Snapshot{}, err
	}
	return d.refresh()
}

func (d *Desktop) call(command string, params map[string]any) (json.RawMessage, error) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	if client == nil {
		return nil, errors.New("Pi 未连接，请先打开项目")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return client.Call(ctx, command, params)
}

func (d *Desktop) refresh() (Snapshot, error) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	if client == nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		return Snapshot{Generation: d.generation, Sequence: d.sequence, Project: d.project}, nil
	}
	for _, command := range []string{"get_state", "get_available_models", "get_messages"} {
		if _, err := d.call(command, nil); err != nil {
			return Snapshot{}, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.transientOverflow {
		return Snapshot{}, errors.New("当前回复的实时恢复缓存已满，请在任务结束后刷新；Pi 会话仍会保存")
	}
	snapshot := d.snapshot
	file, _ := snapshot.State["sessionFile"].(string)
	if file != "" && filepath.Dir(file) == d.sessionDir(d.project) {
		name, _ := snapshot.State["sessionName"].(string)
		if name == "" {
			name = "新会话"
			for _, raw := range snapshot.Messages {
				message, _ := raw.(map[string]any)
				if message["role"] == "user" {
					content, _ := message["content"].(string)
					if blocks, ok := message["content"].([]any); ok {
						for _, rawBlock := range blocks {
							block, _ := rawBlock.(map[string]any)
							if block["type"] == "text" {
								text, _ := block["text"].(string)
								content += text
							}
						}
					}
					content = strings.Join(strings.Fields(content), " ")
					if content != "" {
						name = string([]rune(content)[:min(32, len([]rune(content)))])
						break
					}
				}
			}
		}
		session := localstate.Session{Project: d.project, Path: file, Name: name, Updated: time.Now().UTC().Format(time.RFC3339)}
		d.data.Sessions = append([]localstate.Session{session}, slices.DeleteFunc(slices.Clone(d.data.Sessions), func(s localstate.Session) bool { return s.Path == file })...)
	}
	if err := localstate.Save(filepath.Join(d.dir, "settings.json"), d.data); err != nil {
		return Snapshot{}, fmt.Errorf("无法保存会话索引: %w", err)
	}
	for _, session := range d.data.Sessions {
		if session.Project == d.project {
			snapshot.Sessions = append(snapshot.Sessions, session)
		}
	}
	return snapshot, nil
}

func (d *Desktop) receive(generation int, raw json.RawMessage) {
	var record map[string]any
	if json.Unmarshal(raw, &record) != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if generation != d.generation {
		return
	}
	d.sequence++
	d.rememberTransient(record, len(raw))
	switch record["type"] {
	case "agent_start":
		d.running = true
	case "agent_settled":
		d.running = false
	case "desktop_disconnected":
		d.client = nil
		d.running = false
		d.pending = make(map[string]map[string]any)
	case "extension_ui_resolved":
		id, _ := record["id"].(string)
		delete(d.pending, id)
	case "extension_ui_request":
		id, _ := record["id"].(string)
		if method, _ := record["method"].(string); slices.Contains([]string{"select", "confirm", "input", "editor"}, method) && id != "" {
			d.pending[id] = record
			if timeout, ok := record["timeout"].(float64); ok && timeout > 0 {
				time.AfterFunc(time.Duration(timeout)*time.Millisecond, func() {
					resolved, _ := json.Marshal(map[string]any{"type": "extension_ui_resolved", "id": id})
					d.receive(generation, resolved)
				})
			}
		}
	case "response":
		if record["success"] == true {
			data, _ := record["data"].(map[string]any)
			switch record["command"] {
			case "abort":
				d.running = false
			case "new_session", "switch_session":
				if data["cancelled"] != true {
					d.transient, d.transientOverflow = nil, false
				}
			case "get_state":
				d.snapshot.State = data
				if running, ok := data["isStreaming"].(bool); ok {
					d.running = d.running || running || data["isCompacting"] == true
				}
			case "get_available_models":
				d.snapshot.Models, _ = data["models"].([]any)
			case "get_messages":
				d.snapshot.Messages, _ = data["messages"].([]any)
				d.snapshot.Generation, d.snapshot.Sequence = generation, d.sequence
				d.snapshot.Project, d.snapshot.Connected, d.snapshot.Running = d.project, true, d.running
				d.snapshot.PendingUI = nil
				d.snapshot.Transient = nil
				for _, item := range d.transient {
					d.snapshot.Transient = append(d.snapshot.Transient, item.record)
				}
				for _, request := range d.pending {
					d.snapshot.PendingUI = append(d.snapshot.PendingUI, request)
				}
			}
		}
	}
	entry := PiRecord{generation, d.sequence, string(raw)}
	if d.eventBytes+len(raw) > 4*1024*1024 || len(d.events) >= 1024 {
		// A slow window resynchronizes from Pi instead of stalling stdout.
		entry.Record = `{"type":"desktop_resync"}`
		d.events, d.eventBytes = nil, 0
	}
	d.events = append(d.events, entry)
	d.eventBytes += len(entry.Record)
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// shortcut: keep at most 16 MiB/16384 live records; larger replies need a refresh after settling.
func (d *Desktop) rememberTransient(record map[string]any, size int) {
	kind, _ := record["type"].(string)
	if kind == "agent_settled" {
		d.transient = slices.DeleteFunc(d.transient, func(item transientRecord) bool { return item.record["type"] != "queue_update" })
		d.transientOverflow = false
		return
	}
	if kind == "desktop_disconnected" {
		d.transient, d.transientOverflow = nil, false
		return
	}
	if d.transientOverflow {
		return
	}
	keep := false
	switch kind {
	case "message_start", "message_end":
		d.transient = slices.DeleteFunc(d.transient, func(item transientRecord) bool {
			return item.record["type"] == "message_start" || item.record["type"] == "message_update"
		})
		keep = kind == "message_start"
	case "message_update":
		keep = true
	case "tool_execution_start", "tool_execution_update", "tool_execution_end":
		d.transient = slices.DeleteFunc(d.transient, func(item transientRecord) bool { return item.record["toolCallId"] == record["toolCallId"] })
		keep = kind != "tool_execution_end"
	case "queue_update":
		d.transient = slices.DeleteFunc(d.transient, func(item transientRecord) bool { return item.record["type"] == "queue_update" })
		keep = true
	}
	if !keep {
		return
	}
	bytes := size
	for _, item := range d.transient {
		bytes += item.bytes
	}
	if bytes > 16*1024*1024 || len(d.transient) >= 16384 {
		d.transient, d.transientOverflow = nil, true
		return
	}
	d.transient = append(d.transient, transientRecord{record, size})
}

func (d *Desktop) deliver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
			d.mu.Lock()
			events := d.events
			d.events, d.eventBytes = nil, 0
			d.mu.Unlock()
			for _, event := range events {
				_ = Record.Broadcast(event)
			}
		}
	}
}

func (d *Desktop) sessionDir(project string) string {
	return filepath.Join(d.dir, "sessions", fmt.Sprintf("%x", sha256.Sum256([]byte(project))))
}

func (d *Desktop) ownedSession(project, path string) bool {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(d.sessionDir(project))
	if err != nil || parent != root {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func (d *Desktop) busy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running || d.stopping || d.submitting || len(d.pending) > 0
}

func (d *Desktop) checkQuitting() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quitting {
		return errors.New("应用正在退出")
	}
	return nil
}

func (d *Desktop) disconnect() error {
	d.mu.Lock()
	client := d.client
	d.client = nil
	d.generation++
	d.running = false
	d.pending = make(map[string]map[string]any)
	d.transient, d.transientOverflow = nil, false
	d.mu.Unlock()
	if client != nil {
		return client.Close()
	}
	return nil
}
