import { describe, expect, test } from "bun:test";
import { acceptEnvelope, conversationFromSnapshot, receiveRecord, recoverQueueDraft } from "./session";

describe("Pi conversation events", () => {
  test("cleans ANSI codes only from displayed extension notifications and status", () => {
    const colored = "\u001b[38;5;141mponytail\u001b[0m";
    const notification = { type: "extension_ui_request", method: "notify", message: colored };
    const status = { type: "extension_ui_request", method: "setStatus", statusText: colored };
    let state = conversationFromSnapshot([], false, []);
    state = receiveRecord(state, notification);
    expect(state.notice).toBe("ponytail");
    state = receiveRecord(state, status);
    expect(state.notice).toBe("ponytail");
    expect(notification.message).toBe(colored);
    expect(status.statusText).toBe(colored);
    state = receiveRecord(state, { type: "message_end", message: { role: "assistant", content: colored } });
    expect(state.messages[0]?.content[0]?.text).toBe(colored);
  });

  test("keeps buffered gaps and explicit resync markers blocked until another snapshot", () => {
    const base = { generation: 2, sequence: 10, connected: true, needsSync: false };
    for (const envelope of [
      { generation: 2, sequence: 12, record: JSON.stringify({ type: "agent_start" }) },
      { generation: 2, sequence: 11, record: JSON.stringify({ type: "desktop_resync" }) },
    ]) {
      const missing = acceptEnvelope(base, envelope);
      expect(missing.needsSync).toBe(true);
      expect(missing.event).toBeUndefined();
      const later = acceptEnvelope(missing, { generation: 2, sequence: 13, record: JSON.stringify({ type: "agent_settled" }) });
      expect(later.needsSync).toBe(true);
      expect(later.event).toBeUndefined();
    }
  });

  test("a buffered disconnect overrides the connected snapshot even after a gap", () => {
    const result = acceptEnvelope({ generation: 2, sequence: 10, connected: true, needsSync: false }, {
      generation: 2, sequence: 12, record: JSON.stringify({ type: "desktop_disconnected", error: "Pi 已退出" }),
    });
    expect(result.connected).toBe(false);
    expect(result.needsSync).toBe(true);
    expect(result.event?.type).toBe("desktop_disconnected");
    const stale = acceptEnvelope(result, { generation: 1, sequence: 99, record: JSON.stringify({ type: "agent_start" }) });
    expect(stale.connected).toBe(false);
    expect(stale.event).toBeUndefined();
  });

  test("restores an unfinished message, active tool and queued work before consuming the next delta", () => {
    let state = conversationFromSnapshot([{ role: "user", content: "检查项目" }], true, [], [
      { type: "message_start", message: { role: "assistant", content: [] } },
      { type: "message_update", assistantMessageEvent: { type: "text_delta", contentIndex: 0, delta: "正在检查" } },
      { type: "tool_execution_update", toolCallId: "call-live", toolName: "bash", partialResult: { content: [{ type: "text", text: "running" }] } },
      { type: "queue_update", steering: ["先看配置"], followUp: ["执行测试"] },
    ]);
    expect(state.messages).toHaveLength(2);
    expect(state.messages[1]?.content[0]?.text).toBe("正在检查");
    expect(state.messages[1]?.streaming).toBe(true);
    expect(state.tools["call-live"]?.status).toBe("执行中");
    expect(state.queue).toEqual({ steering: ["先看配置"], followUp: ["执行测试"] });
    state = receiveRecord(state, { type: "message_update", assistantMessageEvent: { type: "text_delta", contentIndex: 0, delta: "项目配置。" } });
    expect(state.messages[1]?.content[0]?.text).toBe("正在检查项目配置。");
    state = receiveRecord(state, { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "配置已检查。" }] } });
    expect(state.messages).toHaveLength(2);
    expect(state.messages[1]?.content[0]?.text).toBe("配置已检查。");
  });

  test("replaces live text with the authoritative message without duplicating it", () => {
    let state = conversationFromSnapshot([], false, []);
    state = receiveRecord(state, { type: "message_start", message: { role: "assistant", content: [] } });
    state = receiveRecord(state, { type: "message_update", assistantMessageEvent: { type: "text_delta", contentIndex: 1, delta: "你好" } });
    expect(state.messages[0]?.content[1]?.text).toBe("你好");
    state = receiveRecord(state, { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "你好，世界" }] } });
    expect(state.messages).toHaveLength(1);
    expect(state.messages[0]?.content[0]?.text).toBe("你好，世界");
    expect(state.messages[0]?.streaming).toBe(false);
  });

  test("stays running across agent_end and stops only when the session settles", () => {
    let state = conversationFromSnapshot([], false, []);
    state = receiveRecord(state, { type: "agent_start" });
    state = receiveRecord(state, { type: "agent_end", willRetry: true });
    expect(state.running).toBe(true);
    state = receiveRecord(state, { type: "agent_settled", aborted: true });
    expect(state.running).toBe(false);
    expect(state.phase).toBe("已停止");
  });

  test("uses full queue snapshots and preserves the existing draft when stopping", () => {
    let state = conversationFromSnapshot([], true, []);
    state = receiveRecord(state, { type: "queue_update", steering: ["修改方向"], followUp: ["执行测试"] });
    state = receiveRecord(state, { type: "queue_update", steering: [], followUp: ["执行测试"] });
    expect(state.queue.steering).toEqual([]);
    expect(recoverQueueDraft("原草稿", { steering: ["修改方向"], followUp: ["执行测试"] })).toBe("原草稿\n\n修改方向\n\n执行测试");
  });

  test("restores historical tool results and clears waiting dialogs on disconnect", () => {
    let state = conversationFromSnapshot([{ role: "toolResult", toolCallId: "call-1", toolName: "bash", content: [{ type: "text", text: "done" }], isError: false }], true, [{ id: "ask-1", method: "confirm", title: "Continue?" }]);
    expect(state.tools["call-1"]?.status).toBe("完成");
    expect(state.pending).toHaveLength(1);
    state = receiveRecord(state, { type: "tool_execution_start", toolCallId: "call-2", toolName: "bash" });
    state = receiveRecord(state, { type: "message_start", message: { role: "assistant", content: [] } });
    state = receiveRecord(state, { type: "desktop_disconnected", error: "Pi 已退出" });
    expect(state.running).toBe(false);
    expect(state.pending).toHaveLength(0);
    expect(state.error).toBe("Pi 已退出");
    expect(state.messages.at(-1)?.streaming).toBe(false);
    expect(state.tools["call-2"]?.status).toBe("已中断");
  });
});
