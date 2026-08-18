// 命令 web-demo 是 qfy-agent 的消费方演示服务（前后端分离，Go 直接提供
// 静态页面渲染）：模拟公司财务系统接入 agent 网关的完整形态——
//   - 模型列表：读取注册表展示可用模型与能力
//   - 对话：普通 chat 与 SSE 流式（前端 fetch + ReadableStream 解析）
//   - 工具演示：注册 map_column 执行器（模拟"已确认映射"查询），
//     由网关内受控循环自动执行（模型调用 → 工具执行 → 回填 → 最终答案）
//   - 审计面板：OnCall 回调落库到内存存储，页面展示每次调用的留痕
//
// 技术栈：Go（net/http，无框架）+ HTML + htmx + alpine.js（静态资源本地
// vendored 并 go:embed 进二进制，局域网环境不依赖 CDN）。默认端口 8077。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/qfy-agent/qfy-agent/audit"
	"github.com/qfy-agent/qfy-agent/backend"
	"github.com/qfy-agent/qfy-agent/loop"
	"github.com/qfy-agent/qfy-agent/registry"
)

func main() {
	configPath := flag.String("config", "agent/config/models.example.yaml", "模型注册表配置文件路径")
	addr := flag.String("addr", "127.0.0.1:8077", "监听地址")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("读取注册表配置 %s 失败: %v", *configPath, err)
	}
	reg, err := registry.Load(data)
	if err != nil {
		log.Fatalf("加载模型注册表失败: %v", err)
	}
	log.Printf("已加载 %d 个模型: %v", len(reg.List()), modelIDs(reg))

	// 后端客户端与推理循环（非流式 60s：注入策略在本地 4B 模型上需 20-40s 生成）。
	client := backend.NewClient()
	notifier := audit.NewNotifier()
	audits := newAuditStore(200) // 模拟落库：内存保留最近 200 条
	notifier.SetOnCall(audits.OnCall)

	// 工具注册：模拟财务系统的"列名映射"工具（网关内自动执行受控循环，KTD3）。
	tools := loop.NewTools()
	if err := tools.Register("map_column", mapColumnTool(), mapColumnExecutor); err != nil {
		log.Fatalf("注册 map_column 工具失败: %v", err)
	}

	runner := loop.NewRunner(tools,
		loop.WithHTTPClient(&http.Client{Timeout: 60 * time.Second}),
		loop.WithOnCall(notifier.Notify))

	s := &server{
		reg:     reg,
		client:  client,
		runner:  runner,
		tools:   tools,
		audits:  audits,
		notify:  notifier.Notify,
	}
	srv := &http.Server{Addr: *addr, Handler: s.routes()}
	log.Printf("web-demo 监听 %s（页面: GET / ；API: /api/models /api/chat /api/chat/stream /api/audit）", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
