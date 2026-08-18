# qfy-agent

qfy-agent 是一个 Go 多模块项目。仓库根目录通过 `go.work` 组织各个独立模块；每个模块维护自己的 `go.mod`、源码、配置示例和使用文档。

## 模块

| 目录 | Go module | 职责 |
|---|---|---|
| [`agent/`](agent/) | `github.com/qfy-agent/qfy-agent/agent` | OpenAI 兼容的轻量级 AI Agent 网关库与示例服务 |
| [`web-demo/`](web-demo/) | `github.com/qfy-agent/qfy-agent/web-demo` | 消费方接入演示服务（前后端分离：Go 渲染静态页面 + htmx/alpine.js，默认端口 8077） |

## 开发

在仓库根目录执行：

```bash
go build ./agent/... ./web-demo/...
go vet ./agent/... ./web-demo/...
go test ./agent/... ./web-demo/...
go test -race ./agent/... ./web-demo/...
```

新增 Go 模块时：

1. 在新模块目录中创建独立的 `go.mod`。
2. 执行 `go work use ./<模块目录>` 将模块加入根工作区。
3. 在上方模块表中登记模块路径与职责。
4. 将新模块的 build、vet、test 和 race 命令加入根目录验证流程。

启动 Agent 网关示例服务：

```bash
go run ./agent/cmd/qfy-agent-server \
  -config agent/config/models.example.yaml \
  -addr 127.0.0.1:8080
```

启动消费方接入演示台（web-demo，端口 8077）：

```bash
go run ./web-demo \
  -config agent/config/models.example.yaml \
  -addr 127.0.0.1:8077
```

浏览器打开 `http://127.0.0.1:8077`：模型列表、对话（SSE 流式）、工具调用演示（map_column 自动执行）与审计留痕面板。

模块的架构、配置和 API 使用方式见 [`agent/README.md`](agent/README.md) 与 [`web-demo/README.md`](web-demo/README.md)。

`agent` 模块的版本发布与 tag 规则见 [`docs/runbooks/release.md`](docs/runbooks/release.md)；`web-demo` 仅用于示例和验收，不单独发布版本。
