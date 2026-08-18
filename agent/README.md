# qfy-agent

轻量级 AI Agent 网关库（Go）。对外暴露 **OpenAI 兼容规范**的 HTTP API，对内通过"模型能力声明 + 适配层"抹平本地模型的能力差异——包括工具调用能力薄弱的模型。

本模块位于多模块仓库的 `agent/` 目录，Go module 路径保持为 `github.com/qfy-agent/qfy-agent`。本文件中的仓库路径和命令均以仓库根目录为基准。

设计背景：公司财务系统需为"非标准 Excel 列语义识别"接入本地大模型（LM Studio，`http://192.168.1.91:1234/v1`，主力模型 google/gemma-4-e4b）。该模型原生 function calling 能力薄弱，直接对接会使业务代码适配不同模型差异；引入重型 agent 框架又与"轻量、可控、可审计"诉求冲突。qfy-agent 作为薄网关：**任何 OpenAI SDK 可直接接入，消费方无感知后端真实能力**。

## 架构

```
消费方（OpenAI SDK）── OpenAI 规范 HTTP/SSE ──> api 层（/v1/models、/v1/chat/completions）
                                                    │
                                                    ▼
                                              loop 推理循环（轮数上限 3、校验重试 2、上游调用上限 12）
                                                    │
                            ┌───────────────────────┼───────────────────────┐
                            ▼                       ▼                       ▼
                     tooling 工具抹平          schema 校验            audit 审计回调
                     full / partial / none   （轻量内置校验器）      （CallRecord → 消费方落库）
                            │
                            ▼
                     backend 适配层（归一化 / 参数抹平 / typed error）
                            │
                            ▼
                     registry 模型注册表（YAML 能力声明）
                            │
                            ▼
                     OpenAI 兼容后端（LM Studio / Ollama / vLLM 等）
```

分层职责：

| 包 | 职责 |
|---|---|
| `registry` | YAML 模型注册表：能力声明（tool_calling/json_mode/streaming）与 default_params 参数抹平；加载后不可变，并发只读安全 |
| `backend` | OpenAI 兼容后端 HTTP 客户端：请求归一化（参数抹平、模型 ID 翻译）、响应归一化（字段补齐、finish_reason 白名单）、typed error（后端不可用/上游非 2xx/响应畸形） |
| `tooling` | 工具调用三策略抹平：full 透传 / partial 透传失败降级 / none prompt 注入；解析降级链与 arguments schema 校验 |
| `schema` | 内置轻量 JSON schema 校验器（type/properties/required/enum/items 子集），结构化错误（路径 + 类型码） |
| `loop` | 受控推理循环：轮数上限（默认 3）、校验失败重试（最多 2 次）、工具执行编排（per-tool 超时、panic 隔离）、单请求上游调用上限（默认 12） |
| `audit` | 审计记录（CallRecord）与 OnCall 回调；回调以 recover 包裹，panic 不影响请求 |
| `api` | OpenAI 兼容 HTTP 入口：SSE 透传/模拟流、心跳、错误事件；`agent/cmd/qfy-agent-server` 为可运行示例 |

### 工具调用抹平（核心）

外部统一按 OpenAI 规范传 `tools`，内部按模型能力自动选择策略，消费方看到的永远是标准 `tool_calls`：

| 能力 | 策略 |
|---|---|
| `tool_calling: full` | tools 原样透传后端，tool_calls 原样返回 |
| `tool_calling: partial` | 首轮原生透传；后端调用失败或 tool_calls 结构非法时，本轮自动降级为注入策略重试 |
| `tool_calling: none` | 适配层把 tools 描述改写为 prompt 注入（"可用工具 + 必须输出 JSON"约束 + few-shot），模型输出 JSON 经解析、校验后包装为标准 tool_calls |

注入输出解析按序降级：直接 JSON 解析 → 剥离代码围栏 → 括号配对提取。`arguments` 按工具声明的 JSON Schema 校验（必填、类型），校验失败的错误回喂模型重试（最多 2 次），仍失败返回稳定错误——绝不让模型自由输出。

**工具执行双模式**：

- 通过 `loop.Tools.Register` 注册执行函数 → 网关在单次请求内自动执行受控循环（模型调用 → 工具执行 → tool 消息回填 → 再调用，直到最终答案或轮数上限）。
- 只定义未注册执行函数的工具 → 响应返回标准 `tool_calls`，消费方自行执行后以 `role: "tool"` 消息回传（标准 OpenAI 多轮形态）。
- 混合场景（同一响应部分注册部分未注册）→ 整轮返回标准 tool_calls，不部分执行。

### SSE 流式

- 后端支持流式 → 透传真实 SSE 流（逐事件 KTD8 白名单读改写：finish_reason 白名单化、未知字段丢弃、model 回显注册表 ID）。
- 后端不支持流式 → 缓冲完整响应，按 OpenAI chunk 规范模拟流式逐段吐出（content 增量、tool_calls delta 按 index、`data: [DONE]` 结尾）。
- `stream=true` 且带 `tools` → 先走推理循环（上游非流式），再模拟为 SSE 流输出（含 tool_calls delta）。
- 长任务心跳：`15s` 间隔发 `: keep-alive` 注释行防代理/LB 断连；每次写出前 `SetWriteDeadline` 续期（30s），规避 WriteTimeout 截断流。
- 流中错误发标准 error 事件；上游缺 `[DONE]` 视为截断并在审计中标记。

## 配置

模型注册表为 YAML（示例见 `agent/config/models.example.yaml`）：

```yaml
models:
  - id: gemma-4-e4b                 # 对外模型 ID（/v1/models 与请求 model 字段）
    backend: openai-compatible      # 后端类型（第一版仅支持）
    base_url: http://192.168.1.91:1234/v1
    api_key: ""                     # 占位；真实 key 经受保护来源注入，勿提交源码库
    model: google/gemma-4-e4b       # 后端实际模型 id（可与对外 ID 不同）
    capabilities:
      tool_calling: none            # full | partial | none
      json_mode: false              # 后端是否原生支持 response_format.json_object
      streaming: true               # 后端是否支持真实 SSE 流式
    default_params:                 # 参数抹平：外部未显式指定时填充
      temperature: 0.2
      max_tokens: 2048
```

能力字段的行为契约：`streaming: false` 时 `stream=true` 请求走非流式调用 + 缓冲模拟流（不静默透传）；`json_mode: false` 时 `response_format.json_object` 请求返回明确的 `400 json_mode_not_supported`（消费方改用 prompt 约束输出 JSON）。示例配置中两条模型均为 `json_mode: false`——LM Studio 实测不支持 `response_format.type=json_object`（仅接受 `json_schema|text`，2026-08-18 联调确认），能力按后端真实情况如实声明。

库本身**不读取环境变量、不触碰文件系统**（R18）：消费方读取配置文件后调用 `registry.Load` 注入。新增模型只改配置不改代码。

## 快速开始

```bash
# 启动示例服务（默认 127.0.0.1:8080，可 -config 指定配置、-addr 指定监听）
go run ./agent/cmd/qfy-agent-server -config agent/config/models.example.yaml -addr 127.0.0.1:8080
```

```bash
# 1. 列出模型
curl http://127.0.0.1:8080/v1/models

# 2. 普通 chat（非流式）
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gemma-4-e4b","messages":[{"role":"user","content":"用一句话介绍你自己"}]}'

# 3. 带 tools（tool_calling: none 的模型走注入策略，返回标准 tool_calls）
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gemma-4-e4b",
    "messages": [{"role": "user", "content": "把列「客户名称」映射到标准字段，调用工具"}],
    "tools": [{"type": "function", "function": {
      "name": "map_column",
      "description": "把非标准 Excel 列名映射到标准字段",
      "parameters": {"type": "object", "properties": {
        "column": {"type": "string"},
        "standard_field": {"type": "string"}
      }, "required": ["column", "standard_field"]}
    }}]
  }'

# 4. 流式（SSE）
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gemma-4-e4b","messages":[{"role":"user","content":"用一句话介绍 Go"}],"stream":true}'
```

### 嵌入库的方式

```go
data, _ := os.ReadFile("agent/config/models.example.yaml") // 消费方读取文件，库不碰文件系统
reg, _ := registry.Load(data)
client := backend.NewClient()
notifier := audit.NewNotifier()                       // 审计通知器（落库逻辑由消费方实现）
notifier.SetOnCall(func(rec audit.CallRecord) { /* 消费方落库 */ })
runner := loop.NewRunner(nil, loop.WithOnCall(notifier.Notify)) // 工具执行器注册见下方
handler := api.NewHandler(api.HandlerConfig{
    Registry: reg,
    Runner:   runner,
    Client:   client,
    Notifier: notifier,
})
http.ListenAndServe(":8080", handler)
```

工具执行器注册（网关内自动执行受控循环，KTD3）：

```go
tools := loop.NewTools()
tools.Register("map_column", mapColumnToolDef, func(ctx context.Context, call backend.ToolCall) (string, error) {
    // 消费方业务逻辑：把模型产出的参数映射为标准字段
    return doMapping(call.Function.Arguments)
})
runner := loop.NewRunner(tools, loop.WithOnCall(notifier.Notify))
```

## 审计回调

每次调用（含流式）通过 OnCall 回调回传 `audit.CallRecord`：时间戳、模型、策略（full/partial/none/direct）、输入摘要（消息数、工具列表）、输出摘要（前 N 字符）、耗时、错误、轮次、是否流式、是否截断。库不碰数据库，由消费方落库；回调在请求 goroutine 内同步触发，消费方落库逻辑须自保证并发安全，回调不应 panic。

## 真实联调验证（LM Studio · gemma-4-e4b）

验证时间：2026-08-18；后端：`http://192.168.1.91:1234/v1`（LM Studio，模型 google/gemma-4-e4b，`tool_calling: none`）。

**联调能力校准（重要）**：LM Studio 不支持 `response_format.type=json_object`（实测返回 400 `'response_format.type' must be 'json_schema' or 'text'`）。因此示例配置 `json_mode: false`，网关对 json_object 请求返回明确 `400 json_mode_not_supported`，消费方以 prompt 约束输出 JSON。

**场景 1：普通 chat** ✅ 返回标准响应骨架（id/object/created/model/choices/usage 齐全，finish_reason=stop）。

**场景 2：带 tools → 注入策略 → 标准 tool_calls** ✅

```json
{
  "choices": [{
    "message": {
      "role": "assistant", "content": null,
      "tool_calls": [{
        "id": "call_10a48f3271e7cc53", "type": "function",
        "function": {"name": "map_column", "arguments": "{\"column\":\"客户名称\",\"standard_field\":\"customer_name\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}
```

e4b 无原生工具调用，经注入策略正确输出标准 tool_calls（网关生成 id、arguments 为合法 JSON 字符串、schema 校验通过）。

**场景 3：stream=true → SSE 流（透传）** ✅ 751 个事件：首 chunk 带 `delta.role=assistant`、203 个 content 增量 chunk、末 chunk `finish_reason=stop`、`data: [DONE]` 结尾；chunk 的 `model` 回显注册表 ID。

**场景 4：stream=true + tools → SSE 模拟流（tool_calls delta）** ✅ 事件序列含 `delta.tool_calls[{index,id,type,function.name}]` 首块与 arguments 增量、末 chunk `finish_reason=tool_calls`、`data: [DONE]`。循环/工具执行阶段客户端持续收到 `: keep-alive` 心跳注释行（实测 3 条，循环耗时约 45s）——长循环经代理/LB 不被切断。

**审计样例**（示例服务 stdout）：`{"duration_ms":21143,"error":"","messages":1,"model":"gemma-4-e4b","output":"","round":0,"strategy":"none","stream":false,"tools":["map_column"],"truncated":false}`——注入策略（none）、工具列表、耗时均正确记录。

## 边界与运维注意

- **鉴权/限流/多租户不在库内**：对外暴露 `/v1/chat/completions` 前须由消费方网关加鉴权层；模型 id 限定注册表内，请求无法指定任意后端 URL（SSRF 面已闭合）。
- **TLS**：库不终止 TLS，生产部署由消费方网关终止。
- **凭据**：`api_key` 须经受保护来源注入（配置文件收紧权限或密钥管理），任何日志与审计字段不得包含凭据。
- **工具参数是模型受控的不可信输入**：网关按工具声明的 schema 做 shape 校验，消费方工具内部仍须自行做业务校验。
- **审计负载是敏感数据通道**：消费方落库时须脱敏、设定保留期与访问控制；示例 stdout 打印仅限演示。
- **http.Server 配置**：SSE 长流场景建议 `WriteTimeout: 0`（库内已用 SetWriteDeadline 逐次续期）；代理/LB 的 idle 超时须大于心跳间隔 15s。
- **超时与上限**：非流式调用 30s（示例服务注入 60s——注入策略含 few-shot 的 prompt 在本地 4B 模型上实测需 20-40s 生成，30s 会截断真实调用，联调发现）、流式读取 5m、per-tool 30s、单请求上游调用上限 12、心跳 15s——均可配置。

## 二期范围（明确不做）

- `/v1/embeddings`（配合 nomic-embed 做已确认映射的向量检索记忆）
- MCP 桥接（生态插件形态）
- 多后端协议（Anthropic/Gemini 原生协议）
- `response_format` 的 json_schema（strict）完整支持

## 开发

```bash
go build ./agent/...
go vet ./agent/...
go test ./agent/...
go test -race ./agent/...
```
