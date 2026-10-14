# Vertex AI Proxy

基于 Google Gemini 原生 REST API 规范的代理工具，支持匿名与多节点负载均衡调度。

**免安装、解压即用的绿色软件。** 全面支持 Windows、Linux、macOS 以及 Android 手机等平台。

## ✨ 核心特性

- **原生 Gemini 协议驱动**：完整支持 `/v1beta/models/*` 架构下的聊天（文本/思考/真流式）、工具调用（Function Calling）、多模态输入。
- **全家族自治支持**：针对文本/思考、生图/混模（Imagen）、TTS 语音合成（`FamilyAudio`）实施家族物理隔离与独占调度；
  视频模型（Veo 系）已纳入家族分类与目录展示，标记为「目录可见 / 暂不可用」，请求会被明确拒绝而非误入文本路径。
- **内置反爬突破**：内置 TLS 指纹伪装及 reCAPTCHA token 自动获取，轻松通过 Google 匿名端点校验。
- **内置代理节点池**：内嵌 sing-box 内核，支持批量导入订阅和节点，提供并发竞速功能，有效应对429
- **模型目录一键获取**：管理面板可直接经节点池拉取上游模型目录，按「新模型 / 已有模型 / 已移除模型」分类确认导入或移除；
  逐模型启停、别名、家族与抗截断/非流前缀/尾部兼容开关均可视化编辑，保存即对客户端生效（见[部署指南](部署指南.md)「模型管理」）。
- **可视化管理面板**：提供精美的 Web 后台，无需修改 JSON 文件，在浏览器中即可轻松管理 API 密钥、模型、代理节点和系统设置。
- **高级功能**：支持 Token 计数、Gemini 原生端点透传、假非流输出等。

## 🚀 三步上手

**1. 下载解压**：下载对应平台的压缩包并解压到任意位置。

**2. 一键启动**：
- **Windows**：双击运行 `启动.bat`
- **Linux/macOS**：终端执行 `sh start.sh`
- **Android (Termux)**：终端执行 `sh start.sh`

**3. 配置密钥**：
首次启动时控制台会输出**管理员密码**。使用浏览器访问 `http://127.0.0.1:2156/admin/` 登录管理面板。
进入左侧「密钥」菜单，添加一个自定义的 API Key（如 `sk-mykey123`），必须以 `sk-` 开头，或点击“✨”按钮随机生成。

> **如何使用？**
> 在你的客户端（如 Cherry Studio、ChatBox 等）中，将 API Key 填为刚才设置的 `sk-...`，API 地址填为 `http://127.0.0.1:2156/v1` 即可开始使用！

**完整的分平台部署教程**（包括开机自启、代理配置、手机部署、常见问题解答）见 **[部署指南](部署指南.md)**。

## ☁️ Railway / Render 容器部署

支持使用独立的 `Dockerfile.paas` 无交互启动：GitHub Actions 验证并构建镜像，发布到 GHCR，再由 Railway / Render Web Service 拉取运行。无需持久化磁盘，启动配置、管理员密码与 API Key 从平台环境变量恢复；可选自动导入出口节点。

**部署步骤、变量说明和数据重置边界**见 **[PaaS 部署指南](docs/paas-deployment.md)**；本次实现与验证范围见 **[修改及静态审查说明](docs/paas-change-review.md)**。原有 Dockerfile 与桌面启动方式保持兼容。

## 🛠 自己编译（可选）

如果你想从源码自行编译：

```bash
# Linux / macOS / Android (Termux) 环境本地编译
go build -tags "with_utls with_quic" -o vertex-proxy ./cmd/vproxy

# Windows 环境本地编译
go build -tags "with_utls with_quic" -o vertex-proxy.exe ./cmd/vproxy
```

> **提示**：`-tags "with_utls with_quic"` 为全功能构建标签，开启后可完整支持 Hysteria / Hysteria2 / TUIC 等全协议节点。

交叉编译示例：
```bash
# 在 Linux/macOS 上编译 Linux AMD64 版本（服务器）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "with_utls with_quic" -trimpath -ldflags="-s -w" -o vertex-proxy ./cmd/vproxy
# 在 Linux/macOS 上编译 Android ARM64 版本（Termux 手机）
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -tags "with_utls with_quic" -trimpath -ldflags="-s -w" -o vertex-proxy ./cmd/vproxy

# 在 Windows PowerShell 上编译 Linux AMD64 版本（服务器）
& { $env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"; go build -tags "with_utls with_quic" -trimpath -ldflags="-s -w" -o vertex-proxy ./cmd/vproxy }
# 在 Windows PowerShell 上编译 Android ARM64 版本（Termux 手机）
& { $env:CGO_ENABLED="0"; $env:GOOS="android"; $env:GOARCH="arm64"; go build -tags "with_utls with_quic" -trimpath -ldflags="-s -w" -o vertex-proxy ./cmd/vproxy }
```

## 🧪 本地复现 CI

CI 使用 `go.mod` 中声明的 Go 版本，以及正式构建标签 `with_utls,with_quic`。提交前建议在本地跑一遍以下命令，与 GitHub Actions 完全一致：

```bash
# 1. 格式检查（CI 只报告不改写，未格式化文件会直接失败）
gofmt -l cmd internal scripts     # 期望无输出

# 2. 静态检查
go vet -tags "with_utls with_quic" ./...

# 3. 全量测试（CI 用 -count=1 禁用缓存）
go test -tags "with_utls with_quic" -count=1 -timeout 15m ./...

# 4. 正式 tags 构建
CGO_ENABLED=0 go build -tags "with_utls with_quic" -trimpath -ldflags="-s -w" -o vertex-proxy ./cmd/vproxy
```

可选的本地静态分析（需先 `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0`）：

```bash
golangci-lint run --timeout 10m
```

> 仓库已有 `.golangci.yml`，但启用了 `govet(shadow strict)`、`gochecknoglobals`、`gochecknoinits` 等较严格检查，现有代码仍有存量告警。**CI 中该步骤目前为 `continue-on-error`（仅报告、不阻断 PR）**，待告警清零后再改为强制门禁。

也可以直接启用仓库自带的 pre-commit 门禁（含 gofmt / go vet / staticcheck / 仓库内 `gochecks`）：

```bash
git config core.hooksPath .githooks
```

### CI 平台构建范围

以下目标均已实际验证可构建（`CGO_ENABLED=0` + 正式 tags），并在 PR/push 时由 CI 交叉编译：

| GOOS / GOARCH | 说明 |
|------|------|
| `linux/amd64` | 服务器主目标，附带 `version` 冒烟 |
| `linux/arm64` | ARM 服务器 / 开发板 |
| `windows/amd64` | 输出 `.exe` |
| `android/arm64` | Termux 手机 |
| `darwin/amd64` | Intel Mac |
| `darwin/arm64` | Apple Silicon Mac |

另有 `nightly` workflow 每日定时运行 `go test -race`，也可在 Actions 页面手动触发。

## ⚙️ 配置说明

强烈建议直接使用**管理面板**的「设置」页进行配置修改，所有修改即时生效，无需重启。
如果需要手动修改，配置文件路径为 `config/config.json`：

| 选项 | 默认值 | 说明 |
|------|------|------|
| `port_api` | 2156 | 服务监听端口 |
| `admin_password` | 自动生成 | 管理面板登录密码 |
| `max_retries` | 10 | 请求失败补位轮数（总发射预算 = (max_retries+1) × parallel_pool_size） |
| `parallel_pool_enabled` | true | 是否开启并发竞速节点池 |

> **提示**：在模型名（如 `gemini-3.5-flash`）前加上 `假非流-` 前缀（如 `假非流-gemini-3.5-flash`），可将真实流式请求转为单个 SSE 数据包返回，适合需要流式接口但期望完整响应的场景。

详细配置说明请参阅 [部署指南](部署指南.md#配置修改指南)。

