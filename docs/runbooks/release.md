# 版本发布 Runbook

本仓库采用 Go 多模块结构，各模块独立使用 SemVer 版本。发布操作只在 `main` 分支进行。

## Tag 规则

子目录中的 Go module 必须使用目录前缀 tag：

| 模块 | Module path | Tag 示例 |
|---|---|---|
| `agent/` | `github.com/qfy-agent/qfy-agent/agent` | `agent/v0.1.0` |
| `web-demo/` | `github.com/qfy-agent/qfy-agent/web-demo` | `web-demo/v0.1.0` |

只发布发生变化的模块。`web-demo` 依赖已发布的 `agent` 版本，因此同时发布时先发布 `agent`，再发布 `web-demo`。

`web-demo/go.mod` 只保留已发布的 `agent` 版本依赖，不提交指向 `../agent` 的本地 `replace`；根目录 `go.work` 通过版本化 replace 供本地联调使用，发布模块时不会带入本地路径。

## 发布前检查

1. 确认位于 `main`，工作区为空，并同步远端：

   ```bash
   git switch main
   git pull --ff-only origin main
   git status --short
   ```

2. 确认目标 tag 不存在：

   ```bash
   git tag --list 'agent/v0.1.0'
   git ls-remote --tags origin 'refs/tags/agent/v0.1.0'
   ```

3. 执行全量质量检查：

   ```bash
   go build ./agent/... ./web-demo/...
   go vet ./agent/... ./web-demo/...
   go test ./agent/... ./web-demo/...
   go test -race ./agent/... ./web-demo/...
   ```

4. 发布 `agent` 前执行目标后端能力探测：

   ```bash
   go run ./agent/cmd/qfy-agent-probe \
     -base-url http://192.168.1.91:1234/v1 \
     -model google/gemma-4-e4b \
     -max-tokens 512
   ```

5. 检查 module path 和依赖版本：

   ```bash
   (cd agent && go list -m -f '{{.Path}}')
   (cd web-demo && go list -m -f '{{.Path}}')
   ```

## 创建和推送 Tag

使用 annotated tag，并在说明中概括对外变化：

```bash
git tag -a agent/v0.1.0 -m 'agent v0.1.0'
git push origin agent/v0.1.0
```

发布 `web-demo` 时，先确认其 `go.mod` 依赖的是已发布的 `agent` 版本，然后执行：

```bash
git tag -a web-demo/v0.1.0 -m 'web-demo v0.1.0'
git push origin web-demo/v0.1.0
```

推送后验证远端 tag 指向当前 `main` 提交：

```bash
git rev-parse HEAD
git rev-list -n 1 agent/v0.1.0
git ls-remote --tags origin 'refs/tags/agent/v0.1.0*'
```

## 回滚

已被使用的版本不可移动或覆盖，应提交修复并发布新的 patch 版本，例如 `agent/v0.1.1`。

仅当 tag 尚未被任何消费方使用且确认误发时，才删除本地和远端 tag：

```bash
git tag -d agent/v0.1.0
git push origin :refs/tags/agent/v0.1.0
```

删除后修正代码，再使用新的版本号发布，避免缓存中的旧 tag 与重建 tag 内容不一致。
