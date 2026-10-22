import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from "react";
import { isMyGo } from "mygo-runtime";
import ReactMarkdown from "react-markdown";
import { ArrowDown, ArrowUp, Check, ChevronDown, Copy, FolderOpen, MessageSquare, PanelLeft, Plus, Settings2, Square, Terminal, X } from "lucide-react";
import { Desktop, events } from "./mygo";
import { acceptEnvelope, conversationFromSnapshot, object, receiveRecord, recoverQueueDraft, strings, text, type ChatMessage, type ContentBlock, type Conversation, type Envelope, type EnvelopeResult, type RecordValue, type ToolRun } from "./session";

type Snapshot = Awaited<ReturnType<typeof Desktop.snapshot>>;
const initial = conversationFromSnapshot([], false, []);

function basename(path: string) { return path.split(/[\\/]/).filter(Boolean).at(-1) || "选择项目"; }
function errorText(error: unknown) { return error instanceof Error ? error.message : String(error); }
function pretty(value: unknown) { return typeof value === "string" ? value : JSON.stringify(value, null, 2) || ""; }
function contentText(value: unknown): string {
  const result = object(value);
  return Array.isArray(result.content) ? result.content.map((block) => text(object(block).text)).filter(Boolean).join("\n") : pretty(value);
}

function Modal({ title, children, onDismiss }: { title: string; children: ReactNode; onDismiss: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useEffect(() => { ref.current?.showModal(); }, []);
  return <dialog ref={ref} className="modal" aria-labelledby={titleId} onCancel={(event) => { event.preventDefault(); onDismiss(); }}>
    <div className="modal-heading"><h2 id={titleId}>{title}</h2><button className="icon-button" type="button" aria-label="关闭" onClick={onDismiss}><X size={18} /></button></div>
    {children}
  </dialog>;
}

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  const [failed, setFailed] = useState(false);
  return <button className="copy-button" type="button" onClick={async () => {
    try { await navigator.clipboard.writeText(value); setCopied(true); setFailed(false); setTimeout(() => setCopied(false), 1600); }
    catch { setFailed(true); }
  }} aria-label={failed ? "复制失败，请手动选择文字" : copied ? "已复制" : "复制"} title={failed ? "复制失败，请手动选择文字" : "复制"}>
    {copied ? <Check size={13} /> : <Copy size={13} />}<span>{failed ? "请手动复制" : copied ? "已复制" : "复制"}</span>
  </button>;
}

function Markdown({ value, onError }: { value: string; onError: (message: string) => void }) {
  return <ReactMarkdown skipHtml components={{
    a: ({ href, children }) => <button type="button" className="text-link" title={href} onClick={() => {
      if (href && /^https?:\/\//i.test(href)) void Desktop.openExternal(href).catch((error: unknown) => onError(errorText(error)));
    }}>{children}</button>,
    img: ({ alt }) => <span className="muted">[图片：{alt || "未加载"}]</span>,
    pre: ({ children }) => <pre>{children}</pre>,
  }}>{value}</ReactMarkdown>;
}

function ToolCard({ block, run }: { block: ContentBlock; run?: ToolRun }) {
  const [full, setFull] = useState(false);
  const output = run?.result ? contentText(run.result) : "";
  const args = run?.args ?? block.arguments;
  return <details className={`tool-card${run?.error ? " tool-error" : ""}`} open={run?.error || undefined}>
    <summary><Terminal size={14} /><strong>{run?.name || block.name || "工具"}</strong><span className="tool-status">{run?.status || "准备中"}</span><ChevronDown size={13} /></summary>
    <div className="tool-body">
      {args !== undefined && <pre className="tool-arguments">{pretty(args)}</pre>}
      {output && <><pre>{full ? output : output.slice(0, 12000)}</pre>{output.length > 12000 && <button className="text-link" onClick={() => setFull(!full)}>{full ? "收起输出" : `显示完整输出（${output.length.toLocaleString()} 字符）`}</button>}</>}
      {!output && <span className="muted">{run?.status === "执行中" ? "等待输出…" : "暂无文本输出"}</span>}
    </div>
  </details>;
}

function Message({ message, tools, onError }: { message: ChatMessage; tools: Conversation["tools"]; onError: (message: string) => void }) {
  const value = message.content.filter((block) => block.type === "text").map((block) => block.text || "").join("\n");
  const hasContent = message.content.some((block) => block.type === "text" ? !!block.text?.trim() : block.type === "thinking" ? !!block.thinking?.trim() : block.type === "toolCall" || block.type === "image");
  if (message.role === "toolResult" || (!hasContent && !message.error && !message.streaming)) return null;
  return <article className={`message message-${message.role}`}>
    <div className="message-heading"><span className={message.role === "user" ? "user-avatar" : "pi-avatar"}>{message.role === "user" ? "你" : "π"}</span><strong>{message.role === "user" ? "你" : message.role === "assistant" ? "Pi" : message.role || "消息"}</strong>{value && <CopyButton value={value} />}</div>
    <div className="message-content">
      {message.content.map((block, index) => block.type === "text" ? <Markdown key={index} value={block.text || ""} onError={onError} />
        : block.type === "thinking" ? <details className="thinking" key={index}><summary>思考过程</summary><Markdown value={block.thinking || ""} onError={onError} /></details>
        : block.type === "toolCall" ? <ToolCard key={block.id || index} block={block} run={tools[block.id || ""]} />
        : block.type === "image" ? <p className="muted" key={index}>图片附件</p> : null)}
      {message.error && <p className="message-error">{message.error}</p>}
      {message.streaming && !value && <span className="stream-placeholder">正在生成…</span>}
    </div>
  </article>;
}

function ExtensionDialog({ request, onReply, onError, onStop }: { request: RecordValue; onReply: (response: RecordValue) => Promise<void>; onError: (message: string) => void; onStop: () => Promise<void> }) {
  const [value, setValue] = useState(text(request.prefill));
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState("");
  const method = text(request.method);
  async function reply(response: RecordValue) {
    setBusy(true);
    try { await onReply(response); } catch (error) { setFailure(errorText(error)); onError(errorText(error)); } finally { setBusy(false); }
  }
  return <Modal title={text(request.title) || "Pi 需要你的输入"} onDismiss={() => { if (!busy) void reply({ cancelled: true }); }}>
    {failure && <p role="alert" className="message-error">{failure}</p>}
    {request.message ? <p className="dialog-description">{text(request.message)}</p> : null}
    {method === "select" ? <div className="option-list">{strings(request.options).map((option, index) => <button key={index} disabled={busy} onClick={() => void reply({ value: option })}>{option}</button>)}</div>
      : method === "input" || method === "editor" ? <form onSubmit={(event) => { event.preventDefault(); void reply({ value }); }}>
        <label className="field"><span>{method === "editor" ? "内容" : "输入"}</span>{method === "editor" ? <textarea autoFocus disabled={busy} rows={8} value={value} onChange={(event) => setValue(event.target.value)} /> : <input autoFocus disabled={busy} value={value} placeholder={text(request.placeholder)} onChange={(event) => setValue(event.target.value)} />}</label>
        <div className="modal-actions"><button type="button" className="subtle-button" disabled={busy} onClick={() => void reply({ cancelled: true })}>取消</button><button className="primary-button" disabled={busy}>确认</button></div>
      </form> : <div className="modal-actions"><button className="subtle-button" disabled={busy} onClick={() => void reply({ confirmed: false })}>否</button><button autoFocus className="primary-button" disabled={busy} onClick={() => void reply({ confirmed: true })}>是</button></div>}
    <button className="stop-button dialog-stop" disabled={busy} onClick={async () => { setBusy(true); await onStop(); setBusy(false); }}><Square size={11} fill="currentColor" />停止整个任务</button>
  </Modal>;
}

export default function App() {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [conversation, setConversation] = useState(initial);
  const [projects, setProjects] = useState<string[]>([]);
  const [project, setProject] = useState("");
  const [piPath, setPiPath] = useState("");
  const [approve, setApprove] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [desktopReady, setDesktopReady] = useState(false);
  const [draft, setDraft] = useState("");
  const [behavior, setBehavior] = useState("steer");
  const [newContent, setNewContent] = useState(false);
  const [needsSync, setNeedsSync] = useState(false);
  const generation = useRef(0);
  const sequence = useRef(0);
  const connection = useRef(false);
  const snapshotSequence = useRef(0);
  const buffered = useRef<Envelope[]>([]);
  const hydrating = useRef(true);
  const syncBlocked = useRef(false);
  const syncAttempts = useRef(0);
  const scroller = useRef<HTMLDivElement>(null);
  const followBottom = useRef(true);
  const input = useRef<HTMLTextAreaElement>(null);
  const draftValue = useRef("");
  const previousDraftKey = useRef("");
  const state = object(snapshot?.state);
  const model = object(state.model);
  const sessionPath = text(state.sessionFile);
  const sessionName = text(state.sessionName) || snapshot?.sessions?.find((session) => session.path === sessionPath)?.name || "新会话";
  const draftKey = `pi-gui:draft:${sessionPath || snapshot?.project || "new"}`;
  const isBusy = !!busy;
  const running = conversation.running;
  const connected = snapshot?.connected === true;

  const failSnapshot = useCallback((cause: unknown) => {
    let cursor: EnvelopeResult = { generation: generation.current, sequence: sequence.current, connected: connection.current, needsSync: true };
    const finalEvents: RecordValue[] = [];
    for (const envelope of buffered.current) {
      cursor = acceptEnvelope(cursor, envelope);
      if (cursor.event) finalEvents.push(cursor.event);
    }
    sequence.current = cursor.sequence; connection.current = cursor.connected;
    buffered.current = []; hydrating.current = false; syncBlocked.current = true;
    setConversation((current) => finalEvents.reduce(receiveRecord, current));
    setSnapshot((current) => current ? { ...current, connected: cursor.connected } : current);
    setNeedsSync(true);
    setError(`状态同步失败，请刷新后再发送。${errorText(cause)}`);
  }, []);

  const installSnapshot = useCallback(function install(next: Snapshot) {
    if (next.generation < generation.current || (next.generation === generation.current && next.sequence < snapshotSequence.current)) return;
    generation.current = next.generation;
    snapshotSequence.current = next.sequence;
    let nextConversation = conversationFromSnapshot(next.messages ?? [], next.running, next.pendingUI ?? [], next.transient ?? []);
    let cursor: EnvelopeResult = { generation: next.generation, sequence: next.sequence, connected: next.connected, needsSync: false };
    let nextState = next.state;
    for (const envelope of buffered.current) {
      cursor = acceptEnvelope(cursor, envelope);
      if (cursor.event) {
        nextConversation = receiveRecord(nextConversation, cursor.event);
        if (cursor.event.type === "session_info_changed") nextState = { ...object(nextState), sessionName: text(cursor.event.name) };
      }
    }
    sequence.current = cursor.sequence;
    connection.current = cursor.connected;
    buffered.current = [];
    hydrating.current = false;
    syncBlocked.current = cursor.needsSync;
    setNeedsSync(cursor.needsSync);
    setSnapshot({ ...next, connected: cursor.connected, state: nextState });
    setConversation(nextConversation);
    setProject(next.project || "");
    followBottom.current = true;
    setNewContent(false);
    if (!cursor.needsSync) { syncAttempts.current = 0; return; }
    if (++syncAttempts.current > 3) { setError("事件持续缺失，请刷新状态后再发送。"); return; }
    hydrating.current = true;
    void Desktop.snapshot().then(install).catch(failSnapshot);
  }, [failSnapshot]);

  useEffect(() => {
    if (!isMyGo()) { hydrating.current = false; setError("这是界面预览。请从 Pi GUI 桌面应用打开，以连接本机 Pi。"); return; }
    let mounted = true;
    const off = events.piRecord.on((envelope: Envelope) => {
      if (hydrating.current) { buffered.current.push(envelope); return; }
      const result = acceptEnvelope({ generation: generation.current, sequence: sequence.current, connected: connection.current, needsSync: syncBlocked.current }, envelope);
      sequence.current = result.sequence;
      connection.current = result.connected;
      const event = result.event;
      if (event) {
        setConversation((current) => receiveRecord(current, event));
        if (event.type === "desktop_disconnected") setSnapshot((current) => current ? { ...current, connected: false } : current);
        if (event.type === "session_info_changed") setSnapshot((current) => current ? { ...current, state: { ...object(current.state), sessionName: text(event.name) } } : current);
        if (event.type === "agent_settled") void Desktop.snapshot().then((next) => {
          if (mounted && next.generation === generation.current && next.sequence >= snapshotSequence.current && !hydrating.current) { snapshotSequence.current = next.sequence; setSnapshot({ ...next, connected: connection.current }); }
        }).catch((cause: unknown) => setError(errorText(cause)));
      }
      if (result.needsSync && !syncBlocked.current) {
        syncBlocked.current = true; syncAttempts.current = 0;
        hydrating.current = true; buffered.current = []; setNeedsSync(true);
        void Desktop.snapshot().then((next) => { if (mounted) installSnapshot(next); }).catch((cause: unknown) => { if (mounted) failSnapshot(cause); });
      }
    });
    void (async () => {
      try {
        const settings = await Desktop.getSettings();
        if (!mounted) return;
        setDesktopReady(true); setPiPath(settings.piPath); setProjects(settings.projects ?? []);
        const current = await Desktop.snapshot();
        if (!mounted) return;
        installSnapshot(current);
        setProject(current.project || settings.lastProject);
      } catch (cause) {
        if (mounted) { hydrating.current = false; setError(`桌面连接不可用。请从 Pi GUI 应用打开。${errorText(cause)}`); }
      }
    })();
    return () => { mounted = false; off(); };
  }, [installSnapshot, failSnapshot]);

  useEffect(() => {
    try {
      let value = localStorage.getItem(draftKey) || "";
      const projectKey = `pi-gui:draft:${snapshot?.project}`;
      if (sessionPath && previousDraftKey.current === projectKey) {
        const unsent = localStorage.getItem(projectKey) || "";
        value = [value, unsent].filter(Boolean).join("\n\n");
        localStorage.setItem(draftKey, value);
        localStorage.removeItem(projectKey);
      }
      previousDraftKey.current = draftKey;
      draftValue.current = value;
      setDraft(value);
    } catch { setError("草稿存储不可用，退出前请复制未发送的内容。"); }
  }, [draftKey, sessionPath, snapshot?.project]);

  useEffect(() => {
    if (followBottom.current) scroller.current?.scrollTo({ top: scroller.current.scrollHeight });
    else setNewContent(true);
  }, [conversation.messages, conversation.tools]);

  function changeDraft(value: string) {
    draftValue.current = value;
    setDraft(value);
    try { localStorage.setItem(draftKey, value); } catch { setError("草稿保存失败，退出前请复制未发送的内容。"); }
  }

  async function loadSnapshot(label: string, action: () => Promise<Snapshot>) {
    if (isBusy) return;
    setBusy(label); setError(""); hydrating.current = true; buffered.current = []; syncAttempts.current = 0;
    try { installSnapshot(await action()); return true; }
    catch (cause) {
      setError(errorText(cause));
      try { installSnapshot(await Desktop.snapshot()); } catch (retryError) { failSnapshot(retryError); }
      return false;
    } finally { setBusy(""); }
  }

  async function chooseProject() {
    try { const path = await Desktop.chooseProject(); if (path) { setProject(path); setApprove(false); setSettingsOpen(true); } }
    catch (cause) { setError(errorText(cause)); }
  }

  async function connect() {
    const success = await loadSnapshot("正在连接", () => Desktop.start(project, piPath, approve));
    if (success) {
      setSettingsOpen(false); setSidebarOpen(false);
      try { const settings = await Desktop.getSettings(); setProjects(settings.projects ?? []); setPiPath(settings.piPath); }
      catch (cause) { setError(errorText(cause)); }
    }
  }

  async function send() {
    if (!draft.trim() || isBusy || !connected || needsSync) return;
    const submitted = draft;
    setBusy("正在发送"); setError("");
    try { await Desktop.send(submitted, running ? behavior : ""); if (draftValue.current === submitted) changeDraft(""); followBottom.current = true; }
    catch (cause) { setError(errorText(cause)); }
    finally { setBusy((current) => current === "正在发送" ? "" : current); input.current?.focus(); }
  }

  async function stop() {
    if (busy === "正在停止") return;
    setBusy("正在停止"); setError("");
    try {
      const result = object(await Desktop.stop());
      changeDraft(recoverQueueDraft(draftValue.current, result.queue ?? result));
      if (result.stopError) { setError(text(result.stopError)); return; }
      setConversation((current) => ({ ...current, running: false, phase: "已停止", queue: { steering: [], followUp: [] }, pending: [] }));
    } catch (cause) { setError(errorText(cause)); }
    finally { setBusy((current) => current === "正在停止" ? "" : current); }
  }

  const pending = conversation.pending[0];
  const queueCount = conversation.queue.steering.length + conversation.queue.followUp.length;
  const status = busy || (pending ? "等待你的输入" : connected ? conversation.phase : "未连接");
  const currentError = error || conversation.error;

  return <div className={`app-shell${sidebarOpen ? " sidebar-open" : ""}`}>
    <aside className="sidebar">
      <div className="brand"><span className="brand-mark">π</span><span>Pi <strong>工作台</strong></span><button className="icon-button mobile-only" aria-label="收起侧栏" onClick={() => setSidebarOpen(false)}><X size={17} /></button></div>
      <button className="project-button" onClick={() => void chooseProject()} disabled={!desktopReady || running || isBusy}><FolderOpen size={17} /><span><strong>{basename(snapshot?.project || project)}</strong><small>{snapshot?.project || project || "打开本地文件夹"}</small></span><ChevronDown size={14} /></button>
      {projects.length > 1 && <select className="recent-projects" aria-label="最近项目" value={snapshot?.project || project} disabled={running || isBusy} onChange={(event) => { setProject(event.target.value); setApprove(false); setSettingsOpen(true); }}><option value="">最近项目</option>{projects.map((path) => <option key={path} value={path}>{basename(path)}</option>)}</select>}
      <div className="section-heading"><span>会话</span><button className="icon-button" aria-label="新建会话" title="新建会话" disabled={!connected || running || isBusy} onClick={() => void loadSnapshot("正在新建", () => Desktop.newSession())}><Plus size={17} /></button></div>
      <nav className="sessions" aria-label="项目会话">{snapshot?.sessions?.length ? snapshot.sessions.map((session) => <button key={session.path} className={`session-button${session.path === sessionPath ? " selected" : ""}`} disabled={running || isBusy} onClick={() => { void loadSnapshot("正在恢复", () => Desktop.resume(session.path)); setSidebarOpen(false); }}><MessageSquare size={15} /><span><strong>{session.name || "未命名会话"}</strong><small>{session.updated ? new Date(session.updated).toLocaleDateString("zh-CN", { month: "short", day: "numeric" }) : "新会话"}</small></span></button>) : <p className="sidebar-empty">{connected ? "发送第一条消息后，\n会话会保存在这里。" : "连接项目后，\n在这里继续你的会话。"}</p>}</nav>
      <div className="sidebar-footer"><button className="settings-button" disabled={running || isBusy || !desktopReady} onClick={() => setSettingsOpen(true)}><Settings2 size={16} />连接设置</button><span className="local-label"><span className={`status-dot${connected ? " online" : ""}`} />本机 Pi</span></div>
    </aside>

    <main className="workspace">
      <header className="toolbar"><button className="icon-button mobile-only" aria-label="打开侧栏" onClick={() => setSidebarOpen(true)}><PanelLeft size={18} /></button><div className="conversation-title"><strong>{sessionName}</strong><span className="run-status" role="status"><span className={`status-dot${running ? " working" : connected ? " online" : ""}`} />{status}</span></div>
        {connected && <select className="model-select" aria-label="选择模型" disabled={running || isBusy || !snapshot?.models?.length} value={`${text(model.provider)}/${text(model.id)}`} onChange={(event) => { const selected = snapshot?.models.map(object).find((candidate) => `${text(candidate.provider)}/${text(candidate.id)}` === event.target.value); if (selected) void loadSnapshot("正在切换模型", () => Desktop.setModel(text(selected.provider), text(selected.id))); }}><option value={`${text(model.provider)}/${text(model.id)}`}>{text(model.name) || text(model.id) || "未选择模型"}</option>{snapshot?.models.map(object).filter((candidate) => candidate.id !== model.id || candidate.provider !== model.provider).map((candidate) => <option key={`${text(candidate.provider)}/${text(candidate.id)}`} value={`${text(candidate.provider)}/${text(candidate.id)}`}>{text(candidate.name) || text(candidate.id)}</option>)}</select>}
      </header>

      {currentError && <div className="error-banner" role="alert"><span>{currentError}</span>{needsSync && <button className="text-link" disabled={isBusy} onClick={() => void loadSnapshot("正在同步", () => Desktop.snapshot())}>刷新状态</button>}<button className="icon-button" aria-label="关闭错误提示" onClick={() => { setError(""); setConversation((current) => ({ ...current, error: "" })); }}><X size={15} /></button></div>}
      {conversation.notice && <div className="notice-banner"><span>{conversation.notice}</span><button className="icon-button" aria-label="关闭通知" onClick={() => setConversation((current) => ({ ...current, notice: "" }))}><X size={14} /></button></div>}

      <div className="conversation-scroll" ref={scroller} onScroll={(event) => { const element = event.currentTarget; followBottom.current = element.scrollHeight - element.scrollTop - element.clientHeight < 80; if (followBottom.current) setNewContent(false); }}>
        {conversation.messages.length ? <div className="messages">{conversation.messages.map((message, index) => <Message key={`${snapshot?.generation}/${sessionPath}/${index}`} message={message} tools={conversation.tools} onError={setError} />)}</div>
          : <div className="empty-conversation"><div className="empty-mark">π</div><h1>{connected ? "从一个具体任务开始" : "给你的项目一个工作台"}</h1><p>{connected ? "描述要改的代码，或让 Pi 先读一遍项目。\n执行过程和工具输出会显示在这里。" : "连接本机 Pi，在这里阅读、编写和继续工作。\n会话留在本机，模型配置沿用 Pi。"}</p>{!connected && <button className="primary-button" disabled={!desktopReady || isBusy} onClick={() => { if (project) setSettingsOpen(true); else void chooseProject(); }}><FolderOpen size={16} />{project ? "连接项目" : "选择项目"}</button>}</div>}
      </div>

      <div className="composer-area">{newContent && <button className="new-content" onClick={() => { followBottom.current = true; scroller.current?.scrollTo({ top: scroller.current.scrollHeight, behavior: "smooth" }); setNewContent(false); }}><ArrowDown size={14} />查看新内容</button>}
        {queueCount > 0 && <details className="queue"><summary>{queueCount} 条待执行指令</summary>{conversation.queue.steering.map((message, index) => <p key={`s${index}`}><span>追加</span>{message}</p>)}{conversation.queue.followUp.map((message, index) => <p key={`f${index}`}><span>后续</span>{message}</p>)}</details>}
        <form className="composer" onSubmit={(event) => { event.preventDefault(); void send(); }}>
          <textarea ref={input} aria-label="发给 Pi 的消息" placeholder={connected ? "告诉 Pi，你想完成什么…" : "先连接一个项目"} value={draft} disabled={!connected || isBusy || needsSync} rows={3} onChange={(event) => changeDraft(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" && (event.metaKey || event.ctrlKey) && !event.nativeEvent.isComposing) { event.preventDefault(); void send(); } }} />
          <div className="composer-controls"><span className="composer-hint">⌘ Enter 发送 <span>Enter 换行</span></span><div className="send-controls">{(running || busy === "正在发送") && <><select className="behavior-select" aria-label="运行中消息处理方式" value={behavior} onChange={(event) => setBehavior(event.target.value)} disabled={isBusy}><option value="steer">追加指令</option><option value="followUp">完成后执行</option></select><button type="button" className="stop-button" onClick={() => void stop()} disabled={busy === "正在停止"}><Square size={12} fill="currentColor" />停止</button></>}<button className="send-button" type="submit" aria-label={running ? behavior === "steer" ? "追加指令" : "排队消息" : "发送消息"} disabled={!connected || !draft.trim() || isBusy || needsSync}><ArrowUp size={18} /></button></div></div>
        </form><p className="composer-footnote">{connected ? `${basename(snapshot?.project || "")} 中的工作由本机 Pi 执行` : "连接后可发送消息；未发送的草稿保存在本机"}</p>
      </div>
    </main>

    {settingsOpen && <Modal title="连接本机 Pi" onDismiss={() => { if (!isBusy) setSettingsOpen(false); }}><form onSubmit={(event) => { event.preventDefault(); void connect(); }}>
      <p className="dialog-description">选择项目和 Pi 程序。模型账号继续使用 Pi 的现有配置。</p>
      {currentError && <p role="alert" className="message-error">{currentError}</p>}
      <label className="field"><span>项目目录</span><div className="path-input"><input required value={project} onChange={(event) => { setProject(event.target.value); setApprove(false); }} placeholder="/path/to/project" /><button className="subtle-button" type="button" disabled={isBusy} onClick={() => void chooseProject()}>选择</button></div></label>
      <label className="field"><span>Pi 程序路径</span><input required value={piPath} onChange={(event) => setPiPath(event.target.value)} placeholder="/path/to/pi" spellCheck={false} /><small>在终端执行 command -v pi 可查看路径。</small></label>
      <label className="checkbox-field"><input type="checkbox" checked={approve} onChange={(event) => setApprove(event.target.checked)} /><span>加载此项目的扩展、技能和配置<small>仅对你信任的项目启用。Pi 的文件访问不受项目目录限制。</small></span></label>
      <div className="modal-actions"><button className="subtle-button" type="button" disabled={isBusy} onClick={() => setSettingsOpen(false)}>取消</button><button className="primary-button" disabled={isBusy || !project || !piPath}>{busy || "连接项目"}</button></div>
    </form></Modal>}
    {pending && !settingsOpen && <ExtensionDialog key={text(pending.id)} request={pending} onError={setError} onStop={stop} onReply={async (response) => { await Desktop.respond(text(pending.id), response); setConversation((current) => ({ ...current, pending: current.pending.filter((item) => item.id !== pending.id) })); }} />}
  </div>;
}
