// app.js — web-demo 前端逻辑（Alpine.js 状态管理 + fetch/ReadableStream
// 解析 SSE 流式响应）。
(function () {
  "use strict";

  window.demoApp = function () {
    return {
      models: [],
      model: "",
      useTool: false,
      thinking: false,
      streamMode: true,
      messages: [],
      draft: "",
      busy: false,
      auditRequestID: 0,
      suggestions: [
        "请把列「客户名称」映射到标准字段，调用工具",
        "请把列「金额」和「数量」分别映射到标准字段，调用工具",
        "用一句话介绍你自己",
      ],

      init() {
        this.loadModels();
        this.loadAudit();
        window.setInterval(() => this.loadAudit(), 5000);
      },

      async loadModels() {
        try {
          const r = await fetch("/api/models");
          if (!r.ok) throw new Error("HTTP " + r.status);
          const d = await r.json();
          this.models = d.models || [];
          if (!this.models.some((m) => m.id === this.model)) {
            this.model = this.models[0]?.id || "";
          }
        } catch (e) {
          this.push("system", "模型列表加载失败: " + e.message);
        }
      },

      // 审计面板每 5s 拉取 /api/audit，并支持发送完成后和手动刷新。
      async loadAudit() {
        const el = document.getElementById("audit-list");
        if (!el) return;
        const requestID = ++this.auditRequestID;
        try {
          const r = await fetch("/api/audit");
          const d = await r.json();
          if (requestID !== this.auditRequestID) return;
          el.innerHTML = this.auditRows(d.records || []);
        } catch (e) {
          if (requestID !== this.auditRequestID) return;
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
          thinking: this.thinking,
        };
        const request = this.streamMode ? this.streamChat(payload) : this.completeChat(payload);
        request
          .catch((e) => this.push("system", "请求失败: " + e.message))
          .finally(() => {
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
        const selected = this.models.find((m) => m.id === payload.model);
        const simulated = payload.use_tool || selected?.streaming === false;
        const transport = simulated ? "模拟 SSE" : "SSE";
        let asst = this.push("assistant", "", true, transport); // 流式消息占位
        let done = false;
        let visibleSincePaint = 0;
        const chunksPerPaint = 8;
        for (;;) {
          let result;
          try {
            result = await reader.read();
          } catch (e) {
            asst.failed = true;
            this.push("system", "流式响应读取失败: " + e.message);
            break;
          }
          const { value, done: rd } = result;
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
                if (visibleSincePaint > 0) await this.paintStreamFrame();
                done = true;
                continue;
              }
              if (data.startsWith("{")) {
                try {
                  const chunk = JSON.parse(data);
                  if (chunk.error) {
                    asst.failed = true;
                    this.push("system", chunk.error.message || "上游返回错误");
                  } else {
                    if (this.consumeChunk(asst, chunk)) {
                      asst.chunkCount++;
                      visibleSincePaint++;
                      if (asst.chunkCount === 1 || visibleSincePaint >= chunksPerPaint) {
                        await this.paintStreamFrame();
                        visibleSincePaint = 0;
                      }
                    }
                  }
                } catch (e) {
                  asst.failed = true;
                  this.push("system", "流式响应解析失败");
                }
              }
            }
          }
          if (done) break;
        }
        if (!done) {
          asst.failed = true;
          this.push("system", "流式响应意外中断");
        }
        asst.streaming = false;
        if (!asst.failed && !asst.content && !asst.toolCalls.length) {
          asst.content = "（模型无输出）";
        }
      },

      async completeChat(payload) {
        const resp = await fetch("/api/chat", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || "HTTP " + resp.status);
        const asst = this.push("assistant", data.content || "（模型无输出）", false, "JSON");
        asst.toolCalls = data.tool_calls || [];
      },

      // 按 OpenAI chunk 规范累积 delta（content 增量拼接、tool_calls 按 index 累积）。
      consumeChunk(asst, chunk) {
        const choice = (chunk.choices || [])[0];
        if (!choice) return false;
        const delta = choice.delta || {};
        let changed = false;
        if (delta.content) {
          asst.content += delta.content;
          changed = true;
        }
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
            changed = true;
          }
        }
        return changed;
      },

      async paintStreamFrame() {
        await new Promise((resolve) => requestAnimationFrame(resolve));
        const el = this.$refs.chatLog;
        if (el) el.scrollTop = el.scrollHeight;
      },

      toApiMessages() {
        return this.messages
          .filter((m) => m.role === "user" || (m.role === "assistant" && !m.failed))
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

      push(role, content, streaming, transport) {
        const msg = {
          role,
          content: content || "",
          toolCalls: [],
          streaming: !!streaming,
          transport: transport || "",
          chunkCount: 0,
          failed: false,
        };
        this.messages.push(msg);
        this.$nextTick(() => {
          const el = this.$refs.chatLog;
          if (el) el.scrollTop = el.scrollHeight;
        });
        return this.messages[this.messages.length - 1];
      },

      msgLabel(role) {
        return role === "user" ? "你" : role === "system" ? "系统" : "agent";
      },

      modelsHTML() {
        if (!this.models.length) return '<p class="hint">加载中…</p>';
        return (
          '<table class="mini"><tr><th>模型</th><th>上下文</th><th>工具</th><th>流式</th><th>JSON</th></tr>' +
          this.models
            .map(
              (m) =>
                `<tr><td>${this.escapeHTML(m.id)}</td><td>${m.context_window ? Math.round(m.context_window / 1024) + "k" : "未知"}</td><td>${this.escapeHTML(m.tool_calling)}</td><td>${m.streaming ? "✓" : "—"}</td><td>${m.json_mode ? "✓" : "—"}</td></tr>`
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
              return `<tr><td>${t}</td><td>${this.escapeHTML(r.model)}</td><td>${this.escapeHTML(r.strategy)}</td><td>${r.duration_ms}ms</td><td class="${ok ? "ok" : "bad"}">${ok ? (r.truncated ? "截断" : "成功") : "失败"}</td></tr>`;
            })
            .join("") +
          "</table>"
        );
      },

      escapeHTML(value) {
        return String(value ?? "").replace(/[&<>"']/g, (ch) => ({
          "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
        })[ch]);
      },

    };
  };
})();
