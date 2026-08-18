# web-demo

qfy-agent 的**消费方接入演示服务**：模拟公司财务系统接入 Agent 网关的完整形态。前后端分离架构，Go 直接开发（无独立前端工程），页面由 Go 通过 `go:embed` 渲染静态资源，前端使用 **htmx + alpine.js**（本地 vendored，局域网环境不依赖 CDN）。

默认端口 **8077**。

## 演示内容

| 页面区域 | 能力 | 对应库能力 |
|---|---|---|
| 模型选择/能力面板 | 注册表模型列表与能力声明（tool_calling/json_mode/streaming） | `registry` |
| 对话 | 非流式 chat 与 SSE 流式（前端 `fetch + ReadableStream` 解析 chunk 增量） | `loop` / `api` |
| 工具演示 | `map_column` 执行器注册：模型（`tool_calling: none`）经网关注入策略产出标准 tool_calls，网关自动执行并回填，直到给出最终答案 | `tooling` / `loop` 受控循环 |
| 审计面板 | OnCall 回调落库到内存存储，页面展示每次调用的模型/策略/耗时/状态 | `audit` |

## 启动

```bash
# 仓库根目录（go.work 已包含 agent 与 web-demo 模块）
go run ./web-demo \
  -config agent/config/models.example.yaml \
  -addr 127.0.0.1:8077
```

浏览器打开 `http://127.0.0.1:8077`。

## API

| 端点 | 说明 |
|---|---|
| `GET /` | 演示页面（Go 渲染静态资源） |
| `GET /api/models` | 模型列表与能力声明 |
| `POST /api/chat` | 非流式对话（JSON） |
| `POST /api/chat/stream` | SSE 流式对话（带工具时网关内循环后模拟流） |
| `GET /api/audit` | 审计留痕列表（最近 200 条，新→旧） |
| `GET /api/tools` | 已注册演示工具 |

## 对话请求形态

```json
{
  "model": "gemma-4-e4b",
  "messages": [{"role": "user", "content": "请把列「客户名称」映射到标准字段，调用工具"}],
  "use_tool": true
}
```

`use_tool: true` 时后端注入 `map_column` 工具定义（声明参数 schema），网关按模型能力自动选择策略；执行器已注册 → 网关内自动执行受控循环（轮数上限 3）。

## 工具执行器（消费方写法示范）

库契约：`ToolCall.Function.Arguments` 是**内容为合法 JSON 的字符串**（R10）。执行器须先解包字符串再解析参数：

```go
func mapColumnExecutor(ctx context.Context, call backend.ToolCall) (string, error) {
	var raw string
	if err := json.Unmarshal(call.Function.Arguments, &raw); err != nil {
		return "", fmt.Errorf("arguments 解包失败: %v", err)
	}
	var args struct {
		Column        string `json:"column"`
		StandardField string `json:"standard_field"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "", fmt.Errorf("参数解析失败: %v", err)
	}
	// 查询"已确认映射"并返回结果文本（作为 role=tool 消息回填）
}
```

## 真实联调验证（2026-08-18 · LM Studio · gemma-4-e4b）

- 非流式对话：返回标准响应（finish_reason=stop）。
- **工具循环**：`map_column` 被模型调用 → 网关执行（模拟映射表查询）→ 回填 → 模型给出最终答案（"已将列「客户名称」映射到标准字段 customer_name"）；审计含工具执行概要（名称/耗时/错误）。
- SSE 流式：830 个 chunk 事件 + `data: [DONE]` 结尾。
- 审计面板：每次调用留痕（模型/策略/耗时/错误/工具结果）。

## 开发

```bash
go test ./web-demo/...
go test -race ./web-demo/...
```
