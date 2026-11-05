# NEXUS / OPS

有记忆、有性格，但不擅自操作的轻量 Linux 运维助手。

Go 单进程 + 内嵌网页 + SQLite。模型在远端运行；不需要 Node.js、Redis、浏览器引擎或向量数据库。提供 DSH 类多轮工具能力，**不是 DSH/Cordis 插件宿主**。

> 第一版面向单用户、单台 Linux 服务器。普通账户运行、变更前审批。不要把拥有登录入口等同于已通过安全审计；不建议直接裸露公网或作为不可信多租户平台。

## 部署文档

首次安装、私有仓库拉取、Linux / Windows 交叉编译、systemd 常驻、SSH 隧道、Caddy HTTPS、备份恢复、升级回滚和故障排查，见 **[完整部署指南](docs/DEPLOYMENT.md)**。

GitHub 仓库只保存源码和文档；本项目需要运行 Go 后端，**不能部署到 GitHub Pages**。运行数据、访问令牌、模型和渠道凭据不得提交到仓库，即使仓库是私有的。

## 功能

- 首次启动设置码、初始化向导、强随机访问令牌与会话撤销。
- 深色青紫界面：主机概览、对话、工具审批、长期记忆、人设、时间线、值守和 QQ 设置。
- DeepSeek/OpenAI 风格 HTTP 多轮工具调用，完整工具参数校验、超时、输出与上下文预算。
- 文件读取/目录/搜索/精确编辑、日志读取、系统指标、服务状态和独立 Shell 进程。
- 中文二元词 + BM25 记忆检索；按范围先过滤，记录来源与过期状态，不使用 embedding。
- 可配置人设和主机/用户档案；人设不影响后端权限。
- QQ 官方 Webhook 与 OneBot v11 正向 WebSocket，独立启用，默认试运行；NapCat 单独部署。
- 可选只读巡检、异常和恢复通知；无变化不调用模型。不自主修复或定时执行任意 Shell。

## 从源码构建

需要 Go **1.24 或更高**；推荐最新受支持版本。生产目标是 Linux，Windows 仅用于网页、数据库和协议开发测试。

```sh
go mod download
go test ./...
go build -trimpath -ldflags="-s -w" -o ops-agent ./cmd/ops-agent
```

依赖通过 Go 模块校验。若默认模块代理在当前网络不可访问，可自行选择受信任的 GOPROXY；不要关闭校验来绕过下载错误。

交叉编译：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/ops-agent-linux-amd64 ./cmd/ops-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/ops-agent-linux-arm64 ./cmd/ops-agent
```

## 第一次运行

先准备仅助手账户可写的数据目录，以及允许工具访问的工作目录。**两者分开**：数据目录包含密钥，不应作为工具工作目录。

```sh
./ops-agent serve --data-dir /var/lib/ops-agent --work-dir /srv/ops-work --listen 127.0.0.1:7788
```

另开一个本机管理终端，使用同一数据目录获取设置码：

```sh
./ops-agent setup-code --data-dir /var/lib/ops-agent
```

- 设置码 15 分钟有效，只可使用一次；解锁后的设置会话 30 分钟有效。
- 在 <http://127.0.0.1:7788> 完成向导。先配置模型、人设、记忆及目录范围，QQ 和值守保持关闭。
- 完成后复制访问令牌并安全保存，然后登录。
- 配置模型可跳过。没有模型凭据时 Agent 请求会明确失败，不伪装成联网助手。
- “测试模型连接”会向你填写的服务发送密钥用于鉴权及固定测试消息；只在点击后发生。

忘记令牌：

```sh
./ops-agent token --data-dir /var/lib/ops-agent show
./ops-agent token --data-dir /var/lib/ops-agent reset
```

重置会撤销既有网页会话。CLI 输出含秘密，勿分享、录屏或重定向到普通日志。正常服务启动不打印访问令牌或设置码。

## 服务器后台常驻

仓库提供 [systemd 模板](deploy/ops-agent.service)。以下是**管理员手动执行示例**，程序不会自行创建用户、提权或安装服务：

```sh
sudo useradd --system --home-dir /var/lib/ops-agent --shell /usr/sbin/nologin ops-agent
sudo install -d -o ops-agent -g ops-agent -m 0700 /var/lib/ops-agent
sudo install -d -o ops-agent -g ops-agent -m 0750 /srv/ops-work
sudo install -m 0755 ./ops-agent /usr/local/bin/ops-agent
sudo install -m 0644 deploy/ops-agent.service /etc/systemd/system/ops-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now ops-agent
sudo -u ops-agent /usr/local/bin/ops-agent setup-code --data-dir /var/lib/ops-agent
```

模板默认限制写入范围、128 MB MemoryHigh、256 MB MemoryMax、64 个任务/进程。限制包含服务派生的工具进程，可能导致大型命令被终止；按自己的业务负载调整，**不是资源占用保证**。

查看状态：

```sh
systemctl status ops-agent
journalctl -u ops-agent -n 100 --no-pager
```

模板启用 `NoNewPrivileges`，不允许在 Agent 内通过 sudo 获取额外权限。对系统目录的管理能力应由管理员明确配置账户权限、systemd 可读写路径和应用目录范围，不把助手直接改成 root。

## 远程访问

默认推荐 SSH 隧道：

```sh
ssh -L 7788:127.0.0.1:7788 your-server
```

然后浏览器访问 <http://127.0.0.1:7788>。若本地端口改变，需要让服务的 `--public-url` 与实际浏览器 origin 一致。

需要域名时使用 HTTPS 反向代理，例如 [Caddy 示例](deploy/Caddyfile.example)，并启动：

```sh
ops-agent serve --data-dir /var/lib/ops-agent --work-dir /srv/ops-work \
  --listen 127.0.0.1:7788 --public-url https://ops.example.com
```

代理保留或明确设置同一 `Host`，允许 SSE 长连接；后端不信任任意 `X-Forwarded-*` 来改变身份边界。防火墙不要放行后端端口。HTTPS public URL 下 Cookie 使用 Secure；别在公网使用 HTTP。

## QQ 接入

两个适配器共用 Agent，但会话、渠道身份和群范围各自隔离。**默认关闭、试运行开启**。试运行可以使用已配置的云端模型，但不发 QQ 消息、不执行 Shell 或文件写入，不能把它当成离线模式。

### QQ 官方机器人

1. 在 QQ 开放平台准备机器人应用，获取平台授予的消息权限及应用凭据。
2. 设置页填写 AppID 和 Secret，配置 HTTPS 回调地址 `https://你的域名/api/callback/qq`。
3. 启用后按平台步骤完成回调验证；本应用校验时间戳和 Ed25519 签名，不把普通 HTTP 请求当成 QQ 事件。
4. 通过网页生成配对码，从要授权的 QQ 身份向机器人发送 `/bind CODE`，再在网页确认候选身份。

现有实现仅处理 **C2C 私聊与群 @ 文本事件、单次被动文本回复**。主动巡检通知不自动走官方渠道；留在网页通知中，界面会提示不支持。是否可收发取决于平台权限、回复窗口和额度；不能保证在任意 QQ 群直接使用。

### OneBot / NapCat

1. 单独安装并登录你自己的 NapCat 或其他 OneBot v11 网关。
2. 开启完整正向 WebSocket 接口（同时具备事件和调用能力），设置访问 token。
3. 在本助手填写 WebSocket URL、token 和预期机器人号。凭据放请求头，不放 URL。
4. 主动测试连接；助手会调用 `get_login_info` 核对身份，身份不匹配拒绝上线。
5. 使用与官方渠道独立的网页配对流程绑定管理员。

NapCat 自身的内存、QQ 客户端依赖和平台合规事项由其部署负责，**不包含在 Go 服务的占用指标中**。不要把网关端口裸露公网。

### 渠道权限

- 默认只接受已绑定身份。官方 openID 不是普通 QQ 号，两种渠道不能直接互换身份。
- 群聊需独立允许群和发送者，明确提及或使用约定命令前缀；群聊不提供本地文件或 Shell 工具。
- 命令执行、文件修改和服务变更都必须到网页批准；群里一句“同意”不是执行授权。
- 配对码只能提交候选，最终授权在网页完成。撤销绑定会取消相关任务，发送前和工具执行前重新检查权限。
- 合并连续消息时保留会话与发送者边界，持久化去重。网络中断下不承诺端到端 exactly-once。
- 调用“测试连接”不会发送业务消息；真正上线会连接外部服务并可能将授权输入交给配置的模型。

## 记忆和数据

- SQLite 数据、凭据和访问令牌放在 `--data-dir`，与程序文件分离，升级不会覆盖。
- **访问令牌明文及其校验哈希保存在受限 SQLite 中**，以单一事务保证本机 CLI 和网页轮换的一致性；模型/渠道凭据保存在受限 `credentials.json`。这不是静态加密，整个数据目录及其备份都必须作为秘密保护。Windows 的 chmod 不能替代 NTFS ACL，本地版本仅供开发；Linux 数据目录 0700、数据库 0600。
- 人设、档案、记忆是参考数据，不能改变工具授权。
- 用户主动保存或网页批准后才形成长期记忆；旧事实可标为过时，不能因经常检索而恢复为事实。
- “忘记”删除记忆及检索索引，旧聊天有独立清除入口；不承诺 SSD/数据库备份中的安全擦除。
- 修改/遗忘记忆会使模型回放历史失效；遗忘时取消正在使用旧上下文的活动任务。已发往云端的历史请求无法通过本地删除撤回。
- 被选入模型上下文的消息、工具输出和记忆会发送到配置的模型服务。只使用有权处理的日志和文件，避免秘密。
- 支持常见凭据拦截与已配置秘密的精确脱敏，不是通用 DLP，也不能识别所有业务敏感信息。
- 保留期及数量限制作用于日志/历史；审计结果不靠模型“遗忘曲线”删除。请自行制定备份及保留策略。

## 工具与安全边界

- 工具入口有后端参数和路径验证；所有任意 Shell、文件写入都需具体审批，参数变化需重新批准。
- Shell 是新进程，不保留上一轮 `cd`/变量。取消会终止进程组，但无法回滚已经完成的系统更改。
- 文件工具的目录限制**不是 Shell 沙箱**。获准执行的命令拥有运行账户的操作系统权限。
- 日志读取只支持不超过 1 MiB 的文件，未直接接入 journal；搜索最多读取 8 MiB。文件修改包含内容/身份复核，但原位写入不是断电原子事务，关键配置请先备份。
- 服务账户能访问的文件原则上也可能被同权限进程访问。需要强隔离时使用专用 VM/容器和严格 OS 权限，而不是仅依赖提示词。
- Linux 实例使用 OS 文件锁防并发服务；Windows 开发模式若被强杀可能遗留 instance.lock，确认旧进程退出后再移除。
- 自定义模型地址仅管理员可设置；远端要求 HTTPS，默认不跟随重定向，错误正文不回显密钥。

## 验证

测试默认使用本地模拟模型、QQ 回调、WebSocket 和临时目录，不使用真实凭据或执行生产维护。

```sh
go test ./...
go vet ./...
# 有受支持 race 工具链时：
go test -race ./...
```

Linux 交叉编译不能替代 Linux 运行验证；真实 QQ 接入、systemd 安装和实际资源占用请依据 [验证报告](docs/VERIFICATION.md) 中的已测与未测项。

## 灵感与实现关系

参考 [Aurabot](https://github.com/aureth-labs/dsh-aurabot-qq) 的“记忆检索、人格分离、消息合并、试运行”设计，独立以 Go 实现。本项目未携带“大肥鱼”的人物形象、动画或默认人格，不运行其 DSH 插件，也不复制其娱乐和社交自动化能力。

接口契约见 [docs/API.md](docs/API.md)。
