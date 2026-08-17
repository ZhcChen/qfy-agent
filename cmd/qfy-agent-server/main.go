// 命令 qfy-agent-server 是可运行示例服务（U7 交付物）：
// 读取模型注册表 YAML → 组装库各层（registry/backend/loop/api）→
// 启动 OpenAI 兼容 HTTP 服务（GET /v1/models、POST /v1/chat/completions）。
//
// R18 边界：文件读取只发生在本示例命令内；库本身不读取文件与环境变量，
// 一切配置由消费方加载后注入。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/qfy-agent/qfy-agent/api"
	"github.com/qfy-agent/qfy-agent/audit"
	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/loop"
	"github.com/qfy-agent/qfy-agent/registry"
)

func main() {
	configPath := flag.String("config", "config/models.example.yaml", "模型注册表配置文件路径")
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址")
	flag.Parse()

	// 文件读取在示例命令内（R18：库不触碰文件系统）。
	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("读取注册表配置 %s 失败: %v", *configPath, err)
	}
	reg, err := registry.Load(data)
	if err != nil {
		log.Fatalf("加载模型注册表失败: %v", err)
	}
	log.Printf("已加载 %d 个模型: %v", len(reg.List()), modelIDs(reg))

	// 后端客户端：供 stream=true 且无 tools 时的真实流式透传路径（R11）。
	// 注：推理循环（loop.Runner）内部自行构造其 client 与策略执行器
	// （tooling.Strategies 不在此处重复创建，避免死代码）。
	client := backend.NewClient()

	// 审计回调：每次调用打印一行 JSON 摘要到 stdout（示例落库形态，
	// 消费方可替换为真实落库；回调不得 panic、不得阻塞请求过久，R17/F1）。
	notifier := audit.NewNotifier()
	notifier.SetOnCall(printAudit)
	// 示例不注册工具执行器：带 tools 的请求返回标准 tool_calls，
	// 由消费方执行后以标准 OpenAI 多轮回传（KTD3）。
	runner := loop.NewRunner(nil, loop.WithOnCall(notifier.Notify))

	srv := api.NewServer(api.ServerConfig{
		HandlerConfig: api.HandlerConfig{
			Registry: reg,
			Runner:   runner,
			Client:   client,
			Notifier: notifier,
		},
		Addr: *addr,
	})
	log.Printf("qfy-agent server 监听 %s（OpenAI 兼容 API: GET /v1/models, POST /v1/chat/completions）", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务退出: %v", err)
	}
}

func modelIDs(reg *registry.Registry) []string {
	ms := reg.List()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// printAudit 把审计记录打印为单行 JSON 摘要（R17：每次调用回传记录，库不碰数据库）。
func printAudit(rec audit.CallRecord) {
	line := map[string]any{
		"timestamp":   rec.Timestamp.Format(time.RFC3339Nano),
		"model":       rec.Model,
		"strategy":    rec.Strategy,
		"round":       rec.Round,
		"stream":      rec.Stream,
		"truncated":   rec.Truncated,
		"error":       rec.Error,
		"duration_ms": rec.Duration.Milliseconds(),
		"messages":    rec.Input.MessageCount,
		"tools":       rec.Input.ToolNames,
		"output":      rec.Output.Content,
	}
	b, err := json.Marshal(line)
	if err != nil {
		log.Printf("audit: 摘要序列化失败: %v", err)
		return
	}
	log.Println("audit " + string(b))
}
