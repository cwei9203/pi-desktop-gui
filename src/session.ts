export type RecordValue = Record<string, unknown>;
export type Envelope = { generation: number; sequence: number; record: string };
export type EnvelopeCursor = { generation: number; sequence: number; connected: boolean; needsSync: boolean };
export type EnvelopeResult = EnvelopeCursor & { event?: RecordValue };
export type ContentBlock = { type: string; text?: string; thinking?: string; id?: string; name?: string; arguments?: unknown };
export type ChatMessage = { role: string; content: ContentBlock[]; streaming: boolean; error?: string; toolCallId?: string; toolName?: string };
export type ToolRun = { name: string; args?: unknown; result?: unknown; status: string; error: boolean };
export type Conversation = {
  messages: ChatMessage[];
  tools: Record<string, ToolRun>;
  running: boolean;
  phase: string;
  queue: { steering: string[]; followUp: string[] };
  pending: RecordValue[];
  notice: string;
  error: string;
};

export function object(value: unknown): RecordValue {
  return typeof value === "object" && value !== null && !Array.isArray(value) ? value as RecordValue : {};
}

export function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function displayText(value: unknown): string {
  return text(value).replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "");
}

export function strings(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

export function acceptEnvelope(cursor: EnvelopeCursor, envelope: Envelope): EnvelopeResult {
  const result = { generation: cursor.generation, sequence: cursor.sequence, connected: cursor.connected, needsSync: cursor.needsSync };
  if (envelope.generation !== result.generation || envelope.sequence <= result.sequence) return result;
  let event: RecordValue;
  try { event = object(JSON.parse(envelope.record)); }
  catch { return { ...result, needsSync: true }; }
  const gap = envelope.sequence !== result.sequence + 1 || event.type === "desktop_resync";
  if (event.type === "desktop_disconnected") return { ...result, sequence: envelope.sequence, connected: false, needsSync: result.needsSync || gap, event };
  if (result.needsSync || gap) return { ...result, needsSync: true };
  return { ...result, sequence: envelope.sequence, event };
}

function message(value: unknown, streaming = false): ChatMessage {
  const source = object(value);
  const content = typeof source.content === "string"
    ? [{ type: "text", text: source.content }]
    : Array.isArray(source.content) ? source.content.map((item) => {
      const block = object(item);
      return { type: text(block.type), text: text(block.text), thinking: text(block.thinking), id: text(block.id), name: text(block.name), arguments: block.arguments };
    }) : [];
  return { role: text(source.role), content, streaming, error: text(source.errorMessage), toolCallId: text(source.toolCallId), toolName: text(source.toolName) };
}

function addToolResult(tools: Conversation["tools"], value: unknown): Conversation["tools"] {
  const source = object(value);
  if (source.role !== "toolResult" || typeof source.toolCallId !== "string") return tools;
  return { ...tools, [source.toolCallId]: { ...tools[source.toolCallId], name: text(source.toolName), result: source, status: source.isError ? "失败" : "完成", error: source.isError === true } };
}

function liveMessageIndex(messages: ChatMessage[], role: string): number {
  for (let index = messages.length - 1; index >= 0; index--) {
    const item = messages[index];
    if (item?.streaming && item.role === role) return index;
  }
  return -1;
}

export function conversationFromSnapshot(messages: unknown[], running: boolean, pending: unknown[], transient: unknown[] = []): Conversation {
  const state: Conversation = {
    messages: messages.map((item) => message(item)),
    tools: messages.reduce<Conversation["tools"]>(addToolResult, {}),
    running,
    phase: running ? "执行中" : "就绪",
    queue: { steering: [], followUp: [] },
    pending: pending.map(object),
    notice: "",
    error: "",
  };
  return transient.reduce(receiveRecord, state);
}

export function receiveRecord(state: Conversation, value: unknown): Conversation {
  const event = object(value);
  const type = text(event.type);
  if (type === "agent_start") return { ...state, running: true, phase: "执行中", error: "" };
  if (type === "agent_settled") return { ...state, messages: state.messages.map((item) => ({ ...item, streaming: false })), running: false, phase: event.aborted ? "已停止" : "就绪" };
  if (type === "desktop_disconnected") return {
    ...state,
    messages: state.messages.map((item) => ({ ...item, streaming: false })),
    tools: Object.fromEntries(Object.entries(state.tools).map(([id, tool]) => [id, tool.status === "执行中" ? { ...tool, status: "已中断", error: true } : tool])),
    running: false, phase: "连接已断开", pending: [], error: text(event.error) || "Pi 已退出，请重新连接。",
  };
  if (type === "compaction_start") return { ...state, phase: "整理上下文" };
  if (type === "compaction_end") return { ...state, phase: state.running ? "执行中" : "就绪" };
  if (type === "auto_retry_start") return { ...state, phase: "等待重试", notice: text(event.errorMessage) };
  if (type === "auto_retry_end") return { ...state, phase: state.running ? "执行中" : "就绪", error: event.success === false ? text(event.finalError) : state.error };
  if (type === "extension_error") return { ...state, error: text(event.errorMessage) || text(event.error) || "扩展执行失败。" };
  if (type === "queue_update") return { ...state, queue: { steering: strings(event.steering), followUp: strings(event.followUp) } };
  if (type === "extension_ui_resolved") return { ...state, pending: state.pending.filter((item) => item.id !== event.id) };
  if (type === "extension_ui_request") {
    if (["select", "confirm", "input", "editor"].includes(text(event.method))) {
      return { ...state, pending: [...state.pending.filter((item) => item.id !== event.id), event] };
    }
    if (event.method === "notify") return { ...state, notice: displayText(event.message) };
    if (event.method === "setStatus") return { ...state, notice: displayText(event.statusText) };
    if (event.method === "set_editor_text") return { ...state, notice: "扩展提供了输入内容，可在下方复制。\n" + text(event.text) };
    return state;
  }
  if (type === "message_start") return { ...state, messages: [...state.messages, message(event.message, true)] };
  if (type === "message_end") {
    const final = message(event.message);
    const index = liveMessageIndex(state.messages, final.role);
    const messages = [...state.messages];
    if (index < 0) messages.push(final);
    else messages[index] = final;
    return { ...state, messages, tools: addToolResult(state.tools, event.message), error: final.error || state.error };
  }
  if (type === "message_update") {
    const update = object(event.assistantMessageEvent);
    const index = liveMessageIndex(state.messages, "assistant");
    if (index < 0 || typeof update.contentIndex !== "number" || !Number.isInteger(update.contentIndex) || update.contentIndex < 0 || update.contentIndex > 10000) return state;
    const original = state.messages[index];
    if (!original) return state;
    const content = [...original.content];
    const block = content[update.contentIndex] ?? { type: "" };
    const updateType = text(update.type);
    if (updateType === "text_delta") content[update.contentIndex] = { ...block, type: "text", text: (block.text ?? "") + text(update.delta) };
    else if (updateType === "text_end") content[update.contentIndex] = { type: "text", text: text(update.content) };
    else if (updateType === "thinking_delta") content[update.contentIndex] = { ...block, type: "thinking", thinking: (block.thinking ?? "") + text(update.delta) };
    else if (updateType === "thinking_end") content[update.contentIndex] = { type: "thinking", thinking: text(update.content) };
    else if (updateType === "toolcall_start") content[update.contentIndex] = { type: "toolCall", id: text(update.id), name: text(update.toolName) };
    else if (updateType === "toolcall_end") {
      const call = object(update.toolCall);
      content[update.contentIndex] = { type: "toolCall", id: text(call.id), name: text(call.name), arguments: call.arguments };
    } else return state;
    const messages = [...state.messages];
    messages[index] = { ...original, content };
    return { ...state, messages };
  }
  if (type.startsWith("tool_execution_") && typeof event.toolCallId === "string") {
    const previous = state.tools[event.toolCallId];
    const end = type === "tool_execution_end";
    const tool: ToolRun = {
      name: text(event.toolName) || previous?.name || "工具",
      args: event.args ?? previous?.args,
      result: event.result ?? event.partialResult ?? previous?.result,
      status: end ? event.isError ? "失败" : "完成" : "执行中",
      error: event.isError === true,
    };
    return { ...state, tools: { ...state.tools, [event.toolCallId]: tool } };
  }
  return state;
}

export function recoverQueueDraft(draft: string, queue: unknown): string {
  const value = object(queue);
  return [draft, ...strings(value.steering), ...strings(value.followUp)].filter(Boolean).join("\n\n");
}
