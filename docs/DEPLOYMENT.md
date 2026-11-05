# NEXUS / OPS 部署指南

本文对应当前源码与 `deploy/` 模板，面向**单用户、单台 Linux 服务器**。推荐先使用 **systemd + SSH 隧道**；确实需要域名访问或 QQ 官方回调时，再配置 Caddy HTTPS。

> 将代码上传 GitHub 不等于服务已经上线。GitHub Pages 不能运行本项目的 Go 后端、SQLite 和系统工具。本文是管理员手动执行的说明，不会自动购买服务器、开放端口、配置 DNS 或连接真实模型 / QQ。

## 1. 部署前准备

### 环境要求

| 项目 | 要求 |
| --- | --- |
| 生产系统 | 带 systemd 的 Linux；以下管理命令以 Debian / Ubuntu 为例 |
| CPU 架构 | `x86_64` 对应 `amd64`，`aarch64` 对应 `arm64`；用 `uname -m` 确认 |
| 构建环境 | Go **1.24 或更高**，推荐仍受支持的稳定版本；Git、网络及可信 CA 证书 |
| 运行依赖 | 一个 Go 可执行文件；网页内嵌，SQLite 为纯 Go 实现，不需要 Node.js、Redis、独立数据库或 C 编译器 |
| 管理权限 | 管理员可使用 sudo 安装服务；应用本身使用专用普通账户，不以 root 运行 |
| 网络 | SSH 管理访问；配置远端模型后需允许出站 HTTPS；域名方案另需 DNS 和入站 80 / 443 |
| 资源 | systemd 模板限制 `MemoryHigh=128M`、`MemoryMax=256M`、`TasksMax=64`，包括工具子进程；这不是实际占用或最低配置承诺 |

构建时的内存、CPU、磁盘需求与运行时不同；小规格服务器可在其他机器编译后上传二进制。NapCat / QQ 客户端是独立组件，其资源不包含在上述限制中。

Go 安装见 [官方安装说明](https://go.dev/doc/install)。如果默认模块代理不可访问，只使用自己信任的 `GOPROXY`；不要关闭 Go 校验数据库绕过错误。

### 约定路径

| 路径 | 用途与权限 |
| --- | --- |
| `/usr/local/bin/ops-agent` | 程序文件，root 所有，0755 |
| `/var/lib/ops-agent` | SQLite、模型 / 渠道凭据和令牌等私有数据，ops-agent 所有，0700 |
| `/srv/ops-work` | 初始工具工作目录，ops-agent 所有，0750 |
| `/etc/systemd/system/ops-agent.service` | systemd 单元 |
| `/etc/systemd/system/ops-agent.service.d/` | 可选的本机覆盖配置，不随源码更新覆盖 |
| `/var/backups/ops-agent` | 本文的备份目录，root 所有，0700 |

**私有数据目录与工具工作目录必须分开**。不要把 `/var/lib/ops-agent`、整个主目录或根目录加入工具可访问范围。上传前也要确认源码目录中没有放入真实业务数据。

## 2. 获取源码与构建

### 从私有 GitHub 仓库获取

将 `YOUR_GITHUB_LOGIN` 替换为实际仓库所有者。服务器应使用自己配置的 SSH 身份；仅拉取代码时，优先使用该仓库的**只读 deploy key**，不要复制开发电脑上的全部凭据。

```sh
git clone git@github.com:YOUR_GITHUB_LOGIN/deepseek.git
cd deepseek
git rev-parse HEAD
```

若使用 HTTPS，可先通过 GitHub CLI 的浏览器流程登录，再克隆。**不要把访问令牌写进克隆 URL、脚本、文档或命令历史**。私有仓库无权限时可能显示 `Repository not found`，不一定是仓库不存在。

记下部署的 Git commit，以便复现和回滚。`dist/` 不提交 Git；当前不能假定仓库已提供预编译 Release、Docker 镜像或自动发布流水线。

### 在目标 Linux 上构建

在源码根目录执行；每一步成功后再进行下一步：

```sh
go version
go mod download
go mod verify
go test ./...
go vet ./...
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/ops-agent ./cmd/ops-agent
./dist/ops-agent version
sha256sum dist/ops-agent
```

构建在管理账户下完成，不需要 `sudo go build`。`go.mod` 和 `go.sum` 应一起保留；部署时不执行 `go get -u` 更新依赖。Go 的测试不需要真实模型或 QQ 凭据。

### 从其他系统交叉编译

在 Linux / macOS / Git Bash 中：

```sh
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/ops-agent-linux-amd64 ./cmd/ops-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/ops-agent-linux-arm64 ./cmd/ops-agent
```

在 **Windows PowerShell 的单独终端**中，先完成模块下载和本机测试，再执行：

```powershell
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -ldflags="-s -w" -o dist/ops-agent-linux-amd64 ./cmd/ops-agent
if ($LASTEXITCODE -ne 0) { throw "Linux amd64 build failed" }
$env:GOARCH = "arm64"
go build -trimpath -ldflags="-s -w" -o dist/ops-agent-linux-arm64 ./cmd/ops-agent
if ($LASTEXITCODE -ne 0) { throw "Linux arm64 build failed" }
Get-FileHash dist/ops-agent-linux-amd64, dist/ops-agent-linux-arm64 -Algorithm SHA256
```

这些环境变量仅用于构建；不要在该终端继续执行面向 Windows 的 `go test`。关闭该终端即可结束这次环境设置。

通过已验证主机指纹的 SSH / SCP 传送**正确架构的二进制和 systemd 模板**，并在服务器用 `sha256sum` 核对。下面只是传输示例，需替换管理员和服务器地址：

```sh
scp dist/ops-agent-linux-amd64 deploy/ops-agent.service admin@YOUR_SERVER:~/
```

首次安装章节中的 `./dist/ops-agent`、`deploy/ops-agent.service` 需相应改为实际传入路径。Linux 二进制不能在 Windows 上运行；交叉编译成功也不代表已通过 Linux 运行、权限或资源限制验证。

## 3. 首次安装为 systemd 服务

本节仅用于**首次安装**。如果账户、程序或服务已经存在，先检查现状和备份，升级请跳到第 9 节；不要直接覆盖已有部署或给既有业务账户改变权限。

在服务器执行：

```sh
sudo useradd --system --user-group --home-dir /var/lib/ops-agent --shell /usr/sbin/nologin ops-agent
sudo install -d -o ops-agent -g ops-agent -m 0700 /var/lib/ops-agent
sudo install -d -o ops-agent -g ops-agent -m 0750 /srv/ops-work
sudo install -o root -g root -m 0755 ./dist/ops-agent /usr/local/bin/ops-agent
sudo install -o root -g root -m 0644 deploy/ops-agent.service /etc/systemd/system/ops-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now ops-agent
```

确认服务和监听地址：

```sh
sudo systemctl status ops-agent --no-pager
sudo journalctl -u ops-agent -n 100 --no-pager
sudo ss -lntp 'sport = :7788'
curl --fail --silent --show-error --max-time 5 http://127.0.0.1:7788/api/status
```

应只监听 `127.0.0.1:7788`。未初始化时状态接口返回 HTTP 200，并包含 `"initialized": false` 和版本字段。这是**进程存活 / 初始化状态**，不是模型、QQ、工具权限或业务就绪证明。

不要开放服务器的 7788 端口；防火墙规则调整前先确认 SSH 管理入口不会被阻断。

## 4. SSH 隧道与首次初始化

在自己的电脑新开终端，保持该连接运行：

```sh
ssh -N -L 127.0.0.1:7788:127.0.0.1:7788 admin@YOUR_SERVER
```

在**服务器的管理终端**生成一次性设置码：

```sh
sudo -u ops-agent /usr/local/bin/ops-agent setup-code --data-dir /var/lib/ops-agent --work-dir /srv/ops-work
```

然后在自己的电脑访问 <http://127.0.0.1:7788>：

1. 输入设置码。设置码 15 分钟有效、只能使用一次；解锁后的设置会话 30 分钟有效。
2. 配置受信任的模型服务地址、模型名和 API Key；也可以先跳过模型配置。
3. 配置人设、记忆和工具范围。工作目录使用 `/srv/ops-work`，不要包含私有数据目录。
4. QQ 和值守暂时保持关闭；需要时再单独启用和验证。
5. 完成初始化，安全保存访问令牌，再登录。

“测试模型连接”会将 API Key 用于向指定服务鉴权，并发送测试消息；实际任务会向该服务发送选中的对话、工具输出和记忆。只使用有权交给该服务处理的数据。没有模型配置时任务会失败，不是离线模型模式。

设置码、访问令牌及 CLI 输出均属于秘密，不要截图、提交 Git 或写入普通日志。服务正常启动不会打印这些秘密。

使用 `127.0.0.1` 而不是随意改成 `localhost`。应用严格检查 Host 和浏览器 Origin；如果本地隧道换了端口，必须同步设置服务的 `--public-url`，不能只改浏览器地址。

## 5. 配置入口与权限

| 参数 | 含义 |
| --- | --- |
| `--data-dir` | 私有数据目录。生产固定为 `/var/lib/ops-agent`；所有管理 CLI 必须使用同一目录 |
| `--work-dir` | 初始工作目录，必须已存在；初始化后还需核对网页中的持久化目录配置 |
| `--listen` | HTTP 监听地址，默认 `127.0.0.1:7788` |
| `--public-url` | 浏览器访问的完整 Origin；例如 `https://ops.example.com`，不得包含路径、查询参数或凭据 |

模型、渠道、人设、记忆和工具范围通过网页向导 / 设置页管理。不要自行假定 `DEEPSEEK_API_KEY`、`PORT` 或 `.env` 会被程序自动读取；这里的生产配置依据是 CLI 参数和网页持久化配置。

模板使用 `NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome`，并仅允许写入工作目录和私有数据目录。工具权限同时受以下约束：

- 服务账户的 Linux 文件权限；
- systemd 隔离和可读写路径；
- 应用的目录范围与逐项审批。

文件工具目录限制**不是 Shell 沙箱**。获准执行的 Shell 拥有该账户可用的操作系统权限。不要为了修复权限错误把服务改为 root、给目录 `chmod 777` 或赋予免密 sudo。

需要调整资源时，使用 `sudo systemctl edit ops-agent` 添加本机覆盖，例如：

```ini
[Service]
MemoryHigh=256M
MemoryMax=512M
TasksMax=128
```

这只是调整语法示例，不是容量建议。保存后执行 `sudo systemctl daemon-reload` 和 `sudo systemctl restart ops-agent`。查看生效配置可使用 `sudo systemctl cat ops-agent`；若配置中后来加入秘密，不要公开其输出。

## 6. 可选：Caddy HTTPS 域名访问

只有确实需要远程网页入口或 QQ 官方回调时才启用。应用尚未完成独立安全审计；有 HTTPS 和令牌不代表适合不可信公网多用户使用。

### 6.1 准备域名与入口

1. 将 `ops.example.com` 换成自己的域名，配置正确的 DNS A / AAAA 记录；不要保留不可达的 AAAA 记录。
2. 根据 [Caddy 官方安装说明](https://caddyserver.com/docs/install) 安装 Caddy 及其 systemd 服务。
3. 为常规自动 HTTPS 验证和访问配置入站 TCP 80 / 443，保留 SSH；**不开放 7788**。
4. 下方示例的域名、`--public-url`、代理 Host 和实际浏览器地址必须一致。

### 6.2 设置后端公开 Origin

执行 `sudo systemctl edit ops-agent`，添加：

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/ops-agent serve --data-dir /var/lib/ops-agent --work-dir /srv/ops-work --listen 127.0.0.1:7788 --public-url https://ops.example.com
```

清空原 `ExecStart` 后再设置新值，避免出现多个启动命令。随后：

```sh
sudo systemctl daemon-reload
sudo systemctl restart ops-agent
```

### 6.3 配置 Caddy

先查看并备份现有 `/etc/caddy/Caddyfile`，再使用 `sudoedit /etc/caddy/Caddyfile` **合并**下面站点块；不要覆盖其他站点。仓库也提供 [Caddyfile 示例](../deploy/Caddyfile.example)。

```caddyfile
ops.example.com {
    reverse_proxy 127.0.0.1:7788 {
        header_up Host ops.example.com
        flush_interval -1
    }
}
```

此配置明确传递匹配的 Host，并保持 SSE 更新及时转发。程序不依赖任意 `X-Forwarded-*` 更改身份边界，也不支持部署在 `/ops/` 等子路径。

验证后再加载：

```sh
sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
sudo systemctl reload caddy
curl --fail --silent --show-error --max-time 10 https://ops.example.com/api/status
```

如果 Caddy 尚未启动，先检查配置和日志，再使用 `sudo systemctl enable --now caddy`。不要用 `curl -k` 掩盖证书问题。

后端本机探活必须携带公开 Host：

```sh
curl --fail --silent --show-error --max-time 5 -H 'Host: ops.example.com' http://127.0.0.1:7788/api/status
```

HTTPS 模式下登录 Cookie 带 `Secure`；应通过 HTTPS 域名重新登录，不能继续通过普通 HTTP 隧道登录。需要恢复纯 SSH 方式时，明确移除公开 Origin 覆盖并重启，而不是关闭 Host / Origin 校验。

## 7. 可选：QQ 与值守

建议网页基础功能稳定后再启用。详见 [README 的 QQ 接入说明](../README.md#qq-接入)。

- **QQ 官方**：需要自己的机器人应用、平台权限、HTTPS 回调 `https://ops.example.com/api/callback/qq`，完成平台验证与网页绑定。当前仅支持 C2C 私聊 / 群 @ 文本事件和单次被动回复，不支持主动巡检推送。
- **OneBot / NapCat**：网关单独部署，使用完整的正向 WebSocket 接口和请求头 token。不要把 token 拼入 URL，也不要公开网关端口。连接测试会核对机器人身份。
- **试运行不是离线模式**：可以调用已配置的模型，但不发 QQ 消息、不执行 Shell 或文件写入。
- **值守默认关闭**：只读巡检，不自动修复或执行任意维护脚本；先确认阈值、通知渠道和接收者。

真实连接、回调权限和通知需要在自己的账号及网络环境下单独验收。不要把模拟测试通过当作真实平台接入成功。

## 8. 备份与令牌管理

### 8.1 一致性备份

SQLite 及其 WAL / SHM、`credentials.json` 等共同构成私有数据。**不要在运行中只复制一个数据库文件**。本指南使用短暂停服后备份整个数据目录的方式；请先安排维护窗口，并确认没有仍在执行的工具任务。

在服务器的同一管理终端逐条执行，失败时停止后续步骤并排查：

```sh
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
sudo install -d -o root -g root -m 0700 /var/backups/ops-agent
sudo systemctl stop ops-agent
sudo systemctl status ops-agent --no-pager
```

此时 `status` 显示 `inactive` 时返回非零是正常的。确认服务和其工具子进程均已退出，再继续：

```sh
sudo tar -C /var/lib -czf "/var/backups/ops-agent/data-$STAMP.tar.gz" ops-agent
sudo chmod 0600 "/var/backups/ops-agent/data-$STAMP.tar.gz"
sudo cp -a /usr/local/bin/ops-agent "/var/backups/ops-agent/binary-$STAMP"
sudo cp -a /etc/systemd/system/ops-agent.service "/var/backups/ops-agent/service-$STAMP"
sudo sha256sum "/var/backups/ops-agent/data-$STAMP.tar.gz" "/var/backups/ops-agent/binary-$STAMP"
sudo systemctl start ops-agent
```

还应单独保存 systemd drop-in、Caddy 配置以及对应 Git commit。工具工作目录中的业务文件不包含在上述数据备份中，须按自己的业务策略另行备份。恢复演练、加密异地副本、保留期限和可用磁盘监控也由管理员负责。

备份包含明文可恢复的访问令牌、会话及模型 / 渠道凭据，**不是加密备份**。不要将其提交到 Git、放进网页目录或作为 GitHub Release 附件。

### 8.2 查看或重置访问令牌

只在可信的服务器终端操作，命令会输出秘密：

```sh
sudo -u ops-agent /usr/local/bin/ops-agent token --data-dir /var/lib/ops-agent show
sudo -u ops-agent /usr/local/bin/ops-agent token --data-dir /var/lib/ops-agent reset
```

Go CLI 参数顺序如上：`--data-dir` 放在 `show` / `reset` **之前**。重置撤销旧网页会话；模型 Key、QQ Secret 和 OneBot token 需在各自平台另行轮换，并更新网页配置。

## 9. 升级与回滚

### 升级

1. 记录正在运行版本的 Git commit、二进制校验值和有效服务配置。
2. 在源码目录检查 `git status --short`；不要覆盖未提交修改。获取并选择已经审阅的 commit / tag，不直接在生产机自动追随最新提交。
3. 按第 2 节构建、测试新版本；构建失败时不触碰当前服务。
4. 按第 8 节完成数据、旧二进制和配置备份，记下 `STAMP`。**在备份流程最后的 `systemctl start` 之前暂停，保持服务停止，然后执行下方切换**，使回滚快照与切换时刻一致。

```sh
sudo install -o root -g root -m 0755 ./dist/ops-agent /usr/local/bin/ops-agent.next
sudo mv /usr/local/bin/ops-agent.next /usr/local/bin/ops-agent
sudo systemctl start ops-agent
sudo systemctl status ops-agent --no-pager
sudo journalctl -u ops-agent -n 100 --no-pager
```

执行前确认 `.next` 不是已有的待发布文件。新文件安装完成后才在同一目录替换，避免直接覆盖运行中的二进制。不要删除数据目录、重复初始化或用新模板覆盖本机 drop-in。

按当前访问模式再次检查 `/api/status`，并验证登录、只读任务、审批及数据保留。`version` 命令未必包含 Git commit，因此部署记录中应额外保存 commit 与 SHA256。

### 回滚与恢复

**只换回旧二进制不保证兼容已经变更的数据库**。稳妥恢复应使用同一时间点的数据、二进制和必要配置。恢复会丢失备份之后的状态，可能恢复旧令牌、会话、渠道配置和绑定；先隔离外部访问、停服并保留故障现场。

以下只接受自己生成并检查过的可信备份。先将 `YOUR_BACKUP_TIMESTAMP` 替换为第 8 节记录的实际值；检查文件存在和哈希后再进行恢复：

```sh
RESTORE=YOUR_BACKUP_TIMESTAMP
sudo ls -l "/var/backups/ops-agent/data-$RESTORE.tar.gz" "/var/backups/ops-agent/binary-$RESTORE"
sudo sha256sum "/var/backups/ops-agent/data-$RESTORE.tar.gz" "/var/backups/ops-agent/binary-$RESTORE"
sudo tar -tzf "/var/backups/ops-agent/data-$RESTORE.tar.gz"
```

确认归档只有预期的 `ops-agent/` 数据路径、校验值与记录一致。再逐条执行：

```sh
FAILED=$(date -u +%Y%m%dT%H%M%SZ)
sudo systemctl stop ops-agent
sudo systemctl status ops-agent --no-pager
```

确认已完全停止；保留原数据，不使用 `rm -rf`：

```sh
sudo mv /var/lib/ops-agent "/var/lib/ops-agent.before-restore-$FAILED"
sudo tar -xzf "/var/backups/ops-agent/data-$RESTORE.tar.gz" -C /var/lib
sudo chown -R ops-agent:ops-agent /var/lib/ops-agent
sudo chmod -R go-rwx /var/lib/ops-agent
sudo chmod 0700 /var/lib/ops-agent
sudo install -o root -g root -m 0755 "/var/backups/ops-agent/binary-$RESTORE" /usr/local/bin/ops-agent
```

检查是否需要恢复当时的服务参数和代理配置，并保留当前配置副本，不盲目覆盖其他站点。对于已经初始化过的备份，建议在重新开放访问前轮换应用令牌：

```sh
sudo -u ops-agent /usr/local/bin/ops-agent token --data-dir /var/lib/ops-agent --work-dir /srv/ops-work reset
sudo systemctl daemon-reload
sudo systemctl start ops-agent
```

核对模型 / 渠道凭据、绑定和开关，按需在平台撤销旧凭据。通过健康检查、登录及业务验证后再恢复外部访问；若任何一步失败，保持服务停止并排查，不继续覆盖或删除现场。

## 10. 常见故障

| 现象 | 检查与处理 |
| --- | --- |
| `go: command not found` 或 Go 版本过低 | 安装受支持且满足 Go 1.24+ 的工具链，重新打开终端并检查 `go version` |
| `Repository not found` / `Permission denied (publickey)` | 核对仓库所有者、私有仓库权限及 SSH key；不要把 token 贴到日志或聊天中 |
| `Exec format error` / `203/EXEC` | 核对 Linux / Windows、amd64 / arm64、可执行权限、实际 `ExecStart` 路径和挂载点是否 `noexec` |
| 无法打开数据目录 / `database is locked` | 检查专用账户、目录所有权、是否有第二个实例；所有 CLI 使用同一数据目录，不对运行中的数据库做手工修改 |
| 403“不受信任的访问地址” | 浏览器域名 / 端口、代理 Host、`--public-url` 必须完全匹配；域名模式本机 curl 需加相同 Host |
| 403“跨站请求被拒绝” | 使用配置的浏览器 Origin；不要关闭校验或使用其他域名混合访问 |
| HTTPS 下反复回登录页 | 检查 HTTPS、Cookie、域名与公开 Origin；不要在 Secure Cookie 模式下改用普通 HTTP |
| Caddy 502 / 证书签发失败 | 查后端服务、127.0.0.1:7788、DNS A / AAAA、80 / 443 和 Caddy 日志；验证配置后才 reload |
| 任务长时间无更新 | 检查 SSE 代理缓冲、超时和网络；模型正文按完整轮次更新，不保证逐 token 展示 |
| 模型任务失败但状态接口为 200 | 状态接口不测试模型；核对地址、模型名、Key、出站 HTTPS 和平台权限，再主动测试连接 |
| 服务内存不足或子进程被终止 | 查看 journal 和 systemd 的内存 / 任务限制；根据实测负载调整，不直接去掉全部隔离 |
| 工具提示无权限 | 同时核对 OS 权限、systemd 限制、网页目录范围和审批；不要直接改成 root |
| 设置码过期 / 访问令牌遗失 | 使用同一私有数据目录重新签发设置码或执行 `token ... reset`；已初始化实例不能重新走设置流程 |
| Windows 开发实例遗留 `instance.lock` | 必须先确认旧实例已经退出；只处理自己测试目录中的锁文件，不据此删除 Linux 活跃实例的锁 |

常用日志命令：

```sh
sudo journalctl -u ops-agent -n 100 --no-pager
sudo journalctl -u caddy -n 100 --no-pager
```

日志可能含业务上下文，分享前要脱敏。应用中的文件日志工具不能直接读取 journal；不要为此给运行账户增加不必要的系统权限。

## 11. 上线验收清单与验证边界

- [ ] 记录仓库 commit、构建平台 / Go 版本及二进制 SHA256。
- [ ] 构建、测试和 `go vet` 在自己的构建环境成功，Linux 服务实际启动并经重启验证。
- [ ] 以 `ops-agent` 账户运行，数据和工作目录分开、权限正确，7788 不向公网暴露。
- [ ] 浏览器 Host / Origin 一致；采用域名时 HTTPS 证书有效。
- [ ] `/api/status` 可达且初始化完成；模型连接另行验证，不能只看 HTTP 200。
- [ ] 访问令牌安全保存；文件修改和 Shell 在批准前没有执行。
- [ ] 完成一次备份和隔离环境恢复演练；确认旧令牌 / 渠道配置的恢复影响。
- [ ] QQ / 值守只在需要时启用，并用自己的账号、权限和网络做真实验收。
- [ ] Git 中没有私有数据库、凭据、备份、日志、访问令牌或本地工具目录。

已有测试记录见 [VERIFICATION.md](VERIFICATION.md)。其中的测试日期与环境属于历史记录，**不等于本次部署实测**。本次编写指南时未具备可用 Go / Linux 运行环境，未重新运行 Go 测试，也未实际安装 systemd、签发证书或连接真实模型 / QQ；执行文档后应逐项记录自己的结果。
