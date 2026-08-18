// app.js — web-demo 前端逻辑（alpine.js 状态管理 + fetch/ReadableStream 解析
// SSE 流式；htmx 用于审计面板与模型列表的局部刷新）。
(function () {
  "use strict";

  window.demoApp = function () {
    return {
      models: [],
      model: "",
      useTool: true,
      messages: [],
      draft: "",
      busy: false,
      suggestions: [
        "请把列「客户名称」映射到标准字段，调用工具",
        "请把列「金额」和「数量」分别映射到标准字段，调用工具",
        "用一句话介绍你自己",
      ],

      init() {
        this.loadModels();
        this.loadAudit();
      },

      async loadModels() {
        try {
          const r = await fetch("/api/models");
          const d = await r.json();
          this.models = d.models || [];
          if (!this.model && this.models.length) this.model = this.models[0].id;
        } catch (e) {
          this.push("system", "模型列表加载失败: " + e.message);
        }
      },

      // 审计面板：htmx 每 5s 拉 /api/audit 渲染；此处提供 alpine 兜底渲染。
      async loadAudit() {
        const el = document.getElementById("audit-list");
        if (!el) return;
        try {
          const r = await fetch("/api/audit");
          const d = await r.json();
          el.innerHTML = this.auditRows(d.records || []);
        } catch (e) {
          el.innerHTML = '<p class="hint">审计加载失败</p>';
        }
      },

      send() {
        const text = (this.draft || "").trim();
        if (!text || this.busy) return;
        this.draft = "";
        this.push("user", text);
        this.busy = true;
        const payload = {
          model: this.model,
          messages: this.toApiMessages(),
          use_tool: this.useTool,
        };
        this.streamChat(payload).finally(() => {
          this.busy = false;
          this.loadAudit();
        });
      },

      // POST SSE 流式（fetch + ReadableStream；POST 语义 EventSource 不支持）。
      async streamChat(payload) {
        const resp = await fetch("/api/chat/stream", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        });
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({}));
          this.push("system", "请求失败: " + (err.error || resp.status));
          return;
        }
        if (!resp.body) {
          this.push("system", "响应无流式体");
          return;
        }
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        let asst = this.push("assistant", "", true); // 流式消息占位
        let done = false;
        for (;;) {
          const { value, done: rd } = await reader.read();
          if (rd) break;
          buffer += decoder.decode(value, { stream: true });
          // SSE 事件以空行分隔；data: 行逐条解析。
          let idx;
          while ((idx = buffer.indexOf("\n\n")) >= 0) {
            const event = buffer.slice(0, idx);
            buffer = buffer.slice(idx + 2);
            for (const line of event.split("\n")) {
              if (!line.startsWith("data:")) continue; // 忽略注释/event 字段
              const data = line.slice(5).trim();
              if (data === "[DONE]") {
                done = true;
                continue;
              }
              if (data.startsWith("{")) {
                this.consumeChunk(asst, JSON.parse(data));
              }
            }
          }
          if (done) break;
        }
        asst.streaming = false;
        if (!asst.content && !asst.toolCalls.length) {
          asst.content = "（模型无输出）";
        }
      },

      // 按 OpenAI chunk 规范累积 delta（content 增量拼接、tool_calls 按 index 累积）。
      consumeChunk(asst, chunk) {
        const choice = (chunk.choices || [])[0];
        if (!choice) return;
        const delta = choice.delta || {};
        if (delta.content) asst.content += delta.content;
        if (delta.tool_calls) {
          for (const tc of delta.tool_calls) {
            if (!asst.toolCalls[tc.index]) {
              asst.toolCalls[tc.index] = {
                id: tc.id || "",
                type: tc.type || "function",
                function: { name: tc.function?.name || "", arguments: "" },
              };
            }
            if (tc.function?.name) asst.toolCalls[tc.index].function.name = tc.function.name;
            if (tc.function?.arguments) asst.toolCalls[tc.index].function.arguments += tc.function.arguments;
          }
        }
      },

      toApiMessages() {
        return this.messages
          .filter((m) => m.role === "user" || m.role === "assistant")
          .map((m) => {
            const base = { role: m.role };
            if (m.role === "assistant" && m.toolCalls && m.toolCalls.length) {
              base.content = null;
              base.tool_calls = m.toolCalls;
            } else {
              base.content = m.content || "";
            }
            return base;
          });
      },

      push(role, content, streaming) {
        const msg = {
          role,
          content: content || "",
          toolCalls: [],
          streaming: !!streaming,
        };
        this.messages.push(msg);
        this.$nextTick(() => {
          const el = this.$refs.chatLog;
          if (el) el.scrollTop = el.scrollHeight;
        });
        return msg;
      },

      msgLabel(role) {
        return role === "user" ? "你" : role === "system" ? "系统" : "agent";
      },

      modelsHTML() {
        if (!this.models.length) return '<p class="hint">加载中…</p>';
        return (
          '<table class="mini"><tr><th>模型</th><th>工具</th><th>流式</th><th>JSON</th></tr>' +
          this.models
            .map(
              (m) =>
                `<tr><td>${m.id}</td><td>${m.tool_calling}</td><td>${m.streaming ? "✓" : "—"}</td><td>${m.json_mode ? "✓" : "—"}</td></tr>`
            )
            .join("") +
          "</table>"
        );
      },

      auditRows(records) {
        if (!records.length) return '<p class="hint">暂无审计记录</p>';
        return (
          '<table class="mini"><tr><th>时间</th><th>模型</th><th>策略</th><th>耗时</th><th>状态</th></tr>' +
          records
            .slice(0, 12)
            .map((r) => {
              const t = new Date(r.timestamp).toLocaleTimeString();
              const ok = !r.error;
              return `<tr><td>${t}</td><td>${r.model}</td><td>${r.strategy}</td><td>${r.duration_ms}ms</td><td class="${ok ? "ok" : "bad"}">${ok ? (r.truncated ? "截断" : "成功") : "失败"}</td></tr>`;
            })
            .join("") +
          "</table>"
        );
      },

      auditHTML() {
        return '<p class="hint">由 htmx 自动刷新（5s）</p>';
      },
    };
  };
})();
