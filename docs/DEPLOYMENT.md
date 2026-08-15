# dex-grid 安装部署文档

版本：v0.2 · 适用于 Windows 10/11、Windows Server 2019+、主流 Linux 发行版

程序编译为**单个可执行文件**，部署时只需要：可执行文件、`config.yaml`、密钥环境变量。HTTP 只提供 REST API，不托管前端页面。

---

## 目录

1. [环境要求](#1-环境要求)
2. [获取程序](#2-获取程序)
3. [目录布局](#3-目录布局)
4. [配置](#4-配置)
5. [Lighter 凭证获取](#5-lighter-凭证获取)
6. [Linux 部署](#6-linux-部署)
7. [Windows 部署](#7-windows-部署)
8. [代理配置](#8-代理配置)
9. [公网访问](#9-公网访问)
10. [部署验收清单](#10-部署验收清单)
11. [升级](#11-升级)
12. [备份与恢复](#12-备份与恢复)
13. [故障排查](#13-故障排查)
14. [卸载](#14-卸载)

---

## 1. 环境要求

### 运行环境

| 项 | 要求 |
| --- | --- |
| 操作系统 | Windows 10/11、Windows Server 2019+、Linux（glibc 2.17+ 或 musl） |
| 架构 | amd64 / arm64 |
| 内存 | 最低 256 MB，建议 512 MB |
| 磁盘 | 程序约 30 MB，数据随成交量增长，建议预留 1 GB |
| 网络 | 能访问目标 DEX 的 REST 与 WebSocket 端点；国内环境通常需要代理 |
| 运行时依赖 | **无**。静态编译，不需要装 Go、不需要装 SQLite、不需要 CGO 运行库 |

### 构建环境（仅从源码构建时需要）

| 项 | 要求 |
| --- | --- |
| Go | 1.25+ |
| Node.js | 不需要（前端待定，当前不打包页面） |
| Git | 任意版本 |

### 时间同步

**服务器时间必须准确**。签名交易带时间戳，时间偏差超过交易所容忍范围会导致所有交易被拒。

```bash
# Linux
timedatectl status          # 确认 "System clock synchronized: yes"
sudo timedatectl set-ntp true
```

```powershell
# Windows（管理员）
w32tm /query /status
w32tm /resync
```

---

## 2. 获取程序

### 方式一：下载预编译产物

从 Release 页面下载对应平台的压缩包：

| 平台 | 文件 |
| --- | --- |
| Linux amd64 | `gridbot_<version>_linux_amd64.tar.gz` |
| Linux arm64 | `gridbot_<version>_linux_arm64.tar.gz` |
| Windows amd64 | `gridbot_<version>_windows_amd64.zip` |

解压后包含 `gridbot`（或 `gridbot.exe`）与 `config.example.yaml`。

### 方式二：从源码构建

```bash
git clone https://github.com/Dog-Feng/dex-grid.git
cd dex-grid

go mod download
```

**Linux / macOS**

```bash
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always)" \
  -o dist/gridbot ./cmd/gridbot
```

**Windows PowerShell**

```powershell
$env:CGO_ENABLED = "0"
go build -trimpath `
  -ldflags "-s -w -X main.version=$(git describe --tags --always)" `
  -o dist\gridbot.exe .\cmd\gridbot
```

**交叉编译（一台机器出两个平台的产物）**

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -o dist/gridbot_linux_amd64   ./cmd/gridbot
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -o dist/gridbot_linux_arm64   ./cmd/gridbot
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o dist/gridbot_windows_amd64.exe ./cmd/gridbot
```

> `CGO_ENABLED=0` 是硬性要求。项目用 `modernc.org/sqlite`（纯 Go 实现）正是为了让交叉编译无痛。若误开 CGO，Linux 上会产生 glibc 版本依赖，Windows 上则需要 gcc 工具链。

仓库提供了封装脚本：`scripts/build.sh` 与 `scripts/build.ps1`（若存在），两者输出一致的产物命名。

无脚本时直接：

```bash
CGO_ENABLED=0 go build -trimpath -o gridbot ./cmd/gridbot
```

---

## 3. 目录布局

推荐布局（Linux 与 Windows 结构相同）：

```
gridbot/
├── gridbot(.exe)          # 可执行文件
├── config.yaml            # 密钥与运维（从 config.example.yaml 复制）
├── config/
│   └── lighter-sol.yaml   # 网格参数；config.yaml 里 strategy_file 指向它
├── .env                   # 密钥（可选，见 4.2）；权限 600
├── data/                  # 运行时数据，程序自动创建
│   ├── gridbot.db         # SQLite：策略配置、订单、成交、统计
│   └── gridbot.lock       # 进程锁
└── logs/
    └── gridbot.log        # 日志文件（config.yaml 中 log_file 指定）
```

**推荐安装路径**

| 平台 | 路径 |
| --- | --- |
| Linux | `/opt/gridbot`，数据 `/var/lib/gridbot`，日志 `/var/log/gridbot` |
| Windows | `C:\gridbot`（避免装在 `Program Files`，写权限麻烦） |

`config.yaml` 中 `data_dir` 若填相对路径，基准是**可执行文件所在目录**，不是当前工作目录。这是为了让 Windows 服务（工作目录常被设成 `C:\Windows\System32`）也能正常找到数据。

---

## 4. 配置

### 4.1 config.yaml

```bash
cp config.example.yaml config.yaml
```

编辑关键字段（完整字段说明见 [GRID_CONFIG.md](GRID_CONFIG.md)）：

```yaml
app:
  log_level: info
  log_file: ./logs/gridbot.log
  data_dir: ./data

server:
  addr: "0.0.0.0:8080"
  auth:
    enabled: false
  cors_origins: ["*"]
  ip_whitelist:
    enabled: false
    allow: ["127.0.0.1"]

exchanges:
  - name: lighter
    enabled: true
    network: mainnet
    credentials:
      account_index: ${LIGHTER_ACCOUNT_INDEX}
      api_key_index: ${LIGHTER_API_KEY_INDEX}
      api_key_private_key: ${LIGHTER_API_KEY_PRIVATE_KEY}
    strategy_file: config/lighter-sol.yaml
    autostart: true
```

密钥和监听端口在 `config.yaml`。网格区间、格数、保证金、杠杆写在 `config/lighter-sol.yaml`，`autostart: true` 时进程起来就开网格，不需要页面。

IP 白名单：把 `server.ip_whitelist.enabled` 设为 `true`，并在 `allow` 里填公网 IP 或 CIDR。本机 `127.0.0.1` / `::1` 始终可访问。

### 4.2 密钥注入

密钥**不写进 config.yaml**，通过环境变量注入。三种方式：

**方式一：Shell 导出（临时，适合前台调试）**

```bash
export LIGHTER_ACCOUNT_INDEX=12345
export LIGHTER_API_KEY_INDEX=1
export LIGHTER_API_KEY_PRIVATE_KEY=0x....
```

```powershell
$env:LIGHTER_ACCOUNT_INDEX = "12345"
$env:LIGHTER_API_KEY_INDEX = "1"
$env:LIGHTER_API_KEY_PRIVATE_KEY = "0x...."
```

**方式二：systemd `EnvironmentFile`（Linux 服务，推荐）** —— 见第 6 节。

**方式三：`.env` 文件（Windows 服务场景推荐）**

程序启动时会自动读取可执行文件同目录下的 `.env`（存在才读，已存在的系统环境变量优先级更高）：

```ini
LIGHTER_ACCOUNT_INDEX=12345
LIGHTER_API_KEY_INDEX=1
LIGHTER_API_KEY_PRIVATE_KEY=0x....
```

Windows 服务和计划任务传环境变量很不方便，`.env` 是最省事的做法。代价是密钥落盘，**必须收紧文件权限**：

```powershell
# 仅当前用户可读写，移除继承的权限
icacls .env /inheritance:r /grant:r "$env:USERNAME:(R,W)"
```

```bash
chmod 600 .env && chown gridbot:gridbot .env
```

把 `.env` 加进 `.gitignore`（仓库已配置）。

---

## 5. Lighter 凭证获取

需要三个值：

| 值 | 获取方式 |
| --- | --- |
| `account_index` | 登录 Lighter 后在账户信息中查看，是一个整数 |
| `api_key_index` | 创建 API Key 时指定的槽位，0-254。首个通常用 `1`（`0` 一般留给官方前端） |
| `api_key_private_key` | 创建 API Key 时生成的私钥，**只显示一次** |

创建流程参考 Lighter 官方文档的 System Setup 章节，或使用官方 Python SDK 的 `system_setup.py` 示例生成。

### 重要安全提示

- API Key 私钥只能**签名交易**，不能提币（提币需要 L1 钱包私钥）。即便如此，泄露仍可导致资金被恶意交易耗尽。
- **不要复用官方前端正在使用的 `api_key_index`**。同一个 key 被两处同时使用会导致 nonce 冲突，双方交易都会失败。给本程序单独分配一个槽位。
- 先在**测试网**（`network: testnet`）跑通完整流程再上主网。

---

## 6. Linux 部署

### 6.1 安装

```bash
# 创建专用用户（不允许登录）
sudo useradd -r -s /usr/sbin/nologin gridbot

# 目录
sudo mkdir -p /opt/gridbot /var/lib/gridbot /var/log/gridbot
sudo tar -xzf gridbot_*_linux_amd64.tar.gz -C /opt/gridbot
sudo cp /opt/gridbot/config.example.yaml /opt/gridbot/config.yaml
sudo chown -R gridbot:gridbot /opt/gridbot /var/lib/gridbot /var/log/gridbot
sudo chmod +x /opt/gridbot/gridbot
```

编辑 `/opt/gridbot/config.yaml`，把路径改成绝对路径：

```yaml
app:
  data_dir: /var/lib/gridbot
  log_file: /var/log/gridbot/gridbot.log
```

### 6.2 密钥文件

```bash
sudo tee /etc/gridbot.env >/dev/null <<'EOF'
LIGHTER_ACCOUNT_INDEX=12345
LIGHTER_API_KEY_INDEX=1
LIGHTER_API_KEY_PRIVATE_KEY=0x....
EOF

sudo chown root:gridbot /etc/gridbot.env
sudo chmod 640 /etc/gridbot.env
```

### 6.3 前台试运行

```bash
sudo -u gridbot env $(cat /etc/gridbot.env | xargs) \
  /opt/gridbot/gridbot
```

看到日志 `api listening addr=0.0.0.0:8080` 后，另开终端：

```bash
curl -s http://127.0.0.1:8080/healthz
# 期望：{"ok":true,"data":{"status":"ok"}}
```

Ctrl+C 会停止策略（撤本交易对挂单、保留仓位）并退出。确认无误后再装 systemd。

### 6.4 systemd 服务

创建 `/etc/systemd/system/gridbot.service`：

```ini
[Unit]
Description=dex-grid trading bot
Documentation=https://github.com/Dog-Feng/dex-grid
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=gridbot
Group=gridbot
WorkingDirectory=/opt/gridbot
EnvironmentFile=/etc/gridbot.env
ExecStart=/opt/gridbot/gridbot -config /opt/gridbot/config.yaml

Restart=always
RestartSec=10s
# 优雅退出：给足时间撤单收尾
KillSignal=SIGTERM
TimeoutStopSec=60s

# 日志走 journald，程序自身也写文件
StandardOutput=journal
StandardError=journal

# 安全加固
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths=/var/lib/gridbot /var/log/gridbot

[Install]
WantedBy=multi-user.target
```

启用并启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now gridbot
sudo systemctl status gridbot
sudo journalctl -u gridbot -f
```

> `TimeoutStopSec=60s` 很重要。停止服务时程序需要撤销本交易对挂单（保留仓位），超时被 `SIGKILL` 会留下未撤的挂单。

### 6.5 日志轮转

程序自身不做轮转，交给 logrotate。创建 `/etc/logrotate.d/gridbot`：

```
/var/log/gridbot/*.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
    su gridbot gridbot
}
```

用 `copytruncate` 是因为程序持有文件句柄，不支持 `SIGHUP` 重开日志。

### 6.6 公网访问 API

默认监听 `0.0.0.0:8080`，无鉴权、无反向代理。云厂商安全组与本机防火墙都要放行 TCP 8080。

```bash
# Ubuntu / Debian
sudo ufw allow 8080/tcp
sudo ufw reload

# firewalld（RHEL / CentOS / Rocky）
sudo firewall-cmd --permanent --add-port=8080/tcp
sudo firewall-cmd --reload
```

本机验证：

```bash
curl -s http://127.0.0.1:8080/healthz
```

外网验证（把 `SERVER_IP` 换成公网 IP）：

```bash
curl -s http://SERVER_IP:8080/healthz
curl -s http://SERVER_IP:8080/api/exchanges
```

只想本机访问时，把 `server.addr` 改成 `127.0.0.1:8080`。需要鉴权时再开 `server.auth.enabled`。

---

## 7. Windows 部署

### 7.1 安装

```powershell
New-Item -ItemType Directory -Force -Path C:\gridbot, C:\gridbot\data, C:\gridbot\logs
Expand-Archive -Path gridbot_*_windows_amd64.zip -DestinationPath C:\gridbot -Force
Copy-Item C:\gridbot\config.example.yaml C:\gridbot\config.yaml
```

编辑 `C:\gridbot\config.yaml`：

```yaml
app:
  data_dir: C:\gridbot\data
  log_file: C:\gridbot\logs\gridbot.log
```

### 7.2 密钥

创建 `C:\gridbot\.env`（程序自动加载）：

```ini
LIGHTER_ACCOUNT_INDEX=12345
LIGHTER_API_KEY_INDEX=1
LIGHTER_API_KEY_PRIVATE_KEY=0x....
```

收紧权限：

```powershell
icacls C:\gridbot\.env /inheritance:r /grant:r "$env:USERNAME:(R,W)"
```

### 7.3 前台试运行

```powershell
cd C:\gridbot
.\gridbot.exe
```

另开终端检查：

```powershell
Invoke-RestMethod http://127.0.0.1:8080/healthz
```

前台运行时按 `Ctrl+C` 触发优雅退出（撤本交易对挂单、保留仓位），**不要直接关窗口或用任务管理器结束进程**。

### 7.4 方式一：任务计划程序（无需额外软件）

适合个人机器，开机自启，登录后运行。

```powershell
# 以管理员运行 PowerShell
$action    = New-ScheduledTaskAction -Execute "C:\gridbot\gridbot.exe" `
                                     -Argument "-config C:\gridbot\config.yaml" `
                                     -WorkingDirectory "C:\gridbot"
$trigger   = New-ScheduledTaskTrigger -AtStartup
$principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -RunLevel Highest
$settings  = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries `
                                          -DontStopIfGoingOnBatteries `
                                          -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
                                          -ExecutionTimeLimit (New-TimeSpan -Seconds 0)

Register-ScheduledTask -TaskName "gridbot" -Action $action -Trigger $trigger `
                       -Principal $principal -Settings $settings
```

管理命令：

```powershell
Start-ScheduledTask   -TaskName gridbot
Stop-ScheduledTask    -TaskName gridbot
Get-ScheduledTaskInfo -TaskName gridbot
```

**注意**：`Stop-ScheduledTask` 是强制终止，不走优雅退出。生产环境优先用下面的 NSSM 方式。

### 7.5 方式二：NSSM 注册为 Windows 服务（推荐）

NSSM 支持优雅停止，行为最接近 systemd。

```powershell
# 下载 nssm.exe 放到 C:\gridbot\ 后（管理员执行）
cd C:\gridbot

.\nssm.exe install gridbot "C:\gridbot\gridbot.exe" "-config C:\gridbot\config.yaml"
.\nssm.exe set gridbot AppDirectory       "C:\gridbot"
.\nssm.exe set gridbot DisplayName        "dex-grid trading bot"
.\nssm.exe set gridbot Description        "Multi-DEX grid trading system"
.\nssm.exe set gridbot Start              SERVICE_AUTO_START

# 优雅停止：先发 Ctrl+C，给 60 秒收尾
.\nssm.exe set gridbot AppStopMethodConsole 60000
.\nssm.exe set gridbot AppStopMethodWindow  0
.\nssm.exe set gridbot AppStopMethodThreads 0

# 崩溃自动重启
.\nssm.exe set gridbot AppExit Default Restart
.\nssm.exe set gridbot AppRestartDelay 10000

# 日志重定向（程序自身也写 log_file，这里兜住启动期的输出）
.\nssm.exe set gridbot AppStdout "C:\gridbot\logs\stdout.log"
.\nssm.exe set gridbot AppStderr "C:\gridbot\logs\stderr.log"
.\nssm.exe set gridbot AppRotateFiles 1
.\nssm.exe set gridbot AppRotateBytes 10485760

.\nssm.exe start gridbot
```

管理命令：

```powershell
nssm status  gridbot
nssm restart gridbot
nssm stop    gridbot          # 走优雅退出
nssm edit    gridbot          # 图形界面编辑配置
```

服务以 `LocalSystem` 运行时不会加载用户环境变量，**这正是推荐用 `.env` 文件的原因**。

### 7.6 Windows 防火墙

监听 `0.0.0.0:8080` 时需要开放入站端口：

```powershell
New-NetFirewallRule -DisplayName "gridbot api" -Direction Inbound `
                    -LocalPort 8080 -Protocol TCP -Action Allow
```

---

## 8. 代理配置

国内网络访问部分 DEX 需要代理。两种配置方式，**优先用配置文件**（更明确，且支持 `GET /api/proxy` 探测）。

### 配置文件（推荐）

```yaml
proxy:
  enabled: true
  url: "http://127.0.0.1:7890"        # 或 socks5://127.0.0.1:1080
  health_interval: 60s
```

对 REST 与 WebSocket 同时生效。连通性探测结果见 `GET /api/proxy`。

### 环境变量（兜底）

```bash
export HTTPS_PROXY=http://127.0.0.1:7890
export NO_PROXY=localhost,127.0.0.1
```

```powershell
$env:HTTPS_PROXY = "http://127.0.0.1:7890"
```

需要认证时：`http://user:pass@host:port`，密码含特殊字符要做 URL 编码。

### 部署在服务器上时

若代理跑在另一台机器，注意代理软件通常默认只监听 `127.0.0.1`，需要改成监听 `0.0.0.0` 并做好访问控制。更推荐把程序和代理部署在同一台机器上。

---

## 9. 公网访问

默认即公网可访问：`server.addr = 0.0.0.0:8080`，`auth.enabled = false`，不需要反向代理或 HTTPS。

需要同时放行：

1. 本机防火墙（见 6.6 / 7.6）
2. 云厂商安全组入站 TCP 8080

```yaml
server:
  addr: "0.0.0.0:8080"
  auth:
    enabled: false
  cors_origins: ["*"]
  ip_whitelist:
    enabled: false
    allow: ["127.0.0.1"]
```

访问入口：`http://<公网IP>:8080/healthz` 与 `/api/*`。根路径 `/` 不托管页面。

建议至少打开一项访问控制：

- `server.ip_whitelist.enabled: true`，`allow` 里填你的公网 IP 或 CIDR（本机 `127.0.0.1` / `::1` 始终放行）
- 或 `auth.enabled: true` 并配置 `GRIDBOT_TOKEN`

不需要对外时把 `addr` 改成 `127.0.0.1:8080`。

---

## 10. 部署验收清单

正式投入资金前逐项确认。

### 环境

- [ ] 系统时间已同步（`timedatectl status` / `w32tm /query /status`）
- [ ] `curl http://127.0.0.1:8080/healthz` 返回 `ok`
- [ ] 公网 `curl http://<公网IP>:8080/healthz` 能通（安全组与防火墙已放行 8080）

### 配置

- [ ] `config.yaml` 中没有明文密钥
- [ ] `.env` 或 `EnvironmentFile` 权限已收紧（600 / 640）
- [ ] `strategy_file` 指向的 YAML 存在，区间/保证金/杠杆已核对
- [ ] 公网已打开 `ip_whitelist` 或 Bearer Token，或 `addr` 已改为 `127.0.0.1:8080`
- [ ] `data_dir` 与 `log_file` 路径存在且可写
- [ ] `api_key_index` 没有和 Lighter 官方前端复用
- [ ] `network` 是期望的值（测试阶段应为 `testnet`）

### 服务

- [ ] 服务能开机自启（`systemctl is-enabled gridbot` / 计划任务已注册）
- [ ] 停止服务时走优雅退出，日志中能看到撤单收尾记录
- [ ] 崩溃后能自动重启（可手动 kill 进程验证）
- [ ] 重启后策略配置与运行状态能从数据库恢复

### 功能

- [ ] `GET /api/exchanges` 能列出已启用交易所
- [ ] `GET /api/exchanges/{ex}/symbols` 能列出永续合约
- [ ] `POST /preview` 能算出派生量；过密网格会被手续费校验拦住
- [ ] `PUT /config` 能保存策略，`POST /start` 能启动
- [ ] `POST /cancel-orders` 后该交易对无挂单、仓位不变
- [ ] `POST /stop` 后本交易对无残留挂单，仓位保留；其他市场不受影响

### 风控

- [ ] 已设置止损价（区间外策略默认 `pause` 只是挂起等待，本身不构成保护）
- [ ] 强平价不在网格区间内（保存配置时会提示）
- [ ] 首次实盘资金量控制在可承受损失的范围内

---

## 11. 升级

**升级前必须先停止所有运行中的实例**（`POST /api/exchanges/{ex}/stop` 或停服务），否则新旧版本对状态的理解可能不一致。

### Linux

```bash
sudo systemctl stop gridbot                        # 等待优雅退出完成
sudo cp -a /var/lib/gridbot /var/lib/gridbot.bak   # 备份数据
sudo cp /opt/gridbot/gridbot /opt/gridbot/gridbot.old

sudo tar -xzf gridbot_<new>_linux_amd64.tar.gz -C /tmp
sudo install -o gridbot -g gridbot -m 755 /tmp/gridbot /opt/gridbot/gridbot

sudo systemctl start gridbot
curl -s http://127.0.0.1:8080/healthz
```

### Windows

```powershell
nssm stop gridbot
Copy-Item C:\gridbot\data C:\gridbot\data.bak -Recurse -Force
Copy-Item C:\gridbot\gridbot.exe C:\gridbot\gridbot.exe.old -Force

Expand-Archive gridbot_<new>_windows_amd64.zip -DestinationPath C:\gridbot -Force
nssm start gridbot
Invoke-RestMethod http://127.0.0.1:8080/healthz
```

### 回滚

```bash
sudo systemctl stop gridbot
sudo cp /opt/gridbot/gridbot.old /opt/gridbot/gridbot
sudo rm -rf /var/lib/gridbot && sudo mv /var/lib/gridbot.bak /var/lib/gridbot
sudo systemctl start gridbot
```

数据库变更遵循向前兼容原则（只加列不删列），但跨大版本升级前请阅读 Release Notes 中的迁移说明。

---

## 12. 备份与恢复

需要备份的只有两样：`config.yaml` 和 `data/gridbot.db`。密钥请用密码管理器单独保管。

### 定时备份（Linux）

```bash
sudo tee /usr/local/bin/gridbot-backup.sh >/dev/null <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
BACKUP_DIR=/var/backups/gridbot
STAMP=$(date +%Y%m%d-%H%M%S)
mkdir -p "$BACKUP_DIR"

# SQLite 在线备份，无需停服
sqlite3 /var/lib/gridbot/gridbot.db ".backup '$BACKUP_DIR/gridbot-$STAMP.db'"
cp /opt/gridbot/config.yaml "$BACKUP_DIR/config-$STAMP.yaml"

# 保留 30 天
find "$BACKUP_DIR" -type f -mtime +30 -delete
EOF

sudo chmod +x /usr/local/bin/gridbot-backup.sh
echo "0 3 * * * root /usr/local/bin/gridbot-backup.sh" | sudo tee /etc/cron.d/gridbot-backup
```

若机器上没有 `sqlite3` 命令行工具，停服后直接 `cp` 数据库文件也可以。

### 定时备份（Windows）

```powershell
# scripts/backup.ps1
$stamp = Get-Date -Format "yyyyMMdd-HHmmss"
$dir   = "C:\gridbot\backups"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
Copy-Item C:\gridbot\data\gridbot.db "$dir\gridbot-$stamp.db"
Copy-Item C:\gridbot\config.yaml     "$dir\config-$stamp.yaml"
Get-ChildItem $dir -File | Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-30) } | Remove-Item
```

注册为每日任务：

```powershell
$a = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NoProfile -File C:\gridbot\scripts\backup.ps1"
$t = New-ScheduledTaskTrigger -Daily -At 3am
Register-ScheduledTask -TaskName "gridbot-backup" -Action $a -Trigger $t -RunLevel Highest
```

### 恢复

停服 → 用备份覆盖 `gridbot.db` 与 `config.yaml` → 启动。启动时会自动对账，把数据库状态与交易所真实挂单/仓位对齐。**备份稍旧不是大问题，对账会修正差异**；但若对账发现仓位偏差超过容忍度，实例会停在 error 状态等你确认。

---

## 13. 故障排查

### 启动失败

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `another instance is running (data/gridbot.lock)` | 同一数据目录已有进程 | 确认没有重复启动。若上次是异常终止，确认进程确实不存在后删除 lock 文件 |
| `bind: address already in use` | 端口被占 | 改 `server.addr` 或 `-addr`，或释放占用端口 |
| `exchange lighter: post-only not supported` | 适配器能力声明异常 | 检查是否连到了不支持的市场或错误的网络 |
| `bind: address already in use` | 端口被占 | 改 `server.addr` 或 `-addr`，或释放占用端口 |

### 连接问题

| 现象 | 排查 |
| --- | --- |
| 连不上交易所 | 确认代理配置；确认防火墙未拦截出站；查看 `logs/gridbot.log` |
| WS 频繁重连 | 网络不稳或代理不稳；查看日志中的重连间隔；尝试直连排除代理因素 |
| 行情不更新但连接正常 | 触发一次「重连交易所」；检查是否订阅了不存在的交易对 |

### 交易问题

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 大量 post-only 被拒 | 价格穿过挂单层级，属正常现象 | 观察「待重试」计数是否能回落到 0。持续不降说明行情单边运行过快，考虑放宽格距 |
| 「批量下单失败：接口限流」 | 请求速率超限 | 调低 `rate_limit.rps`，或减少网格数、启用 `max_active_orders` |
| nonce 相关错误反复出现 | `api_key_index` 与其他程序（含官方前端）冲突 | 换一个独立的 `api_key_index` |
| 挂单数少于「挂单目标」 | 部分下单失败 | `POST /api/exchanges/{ex}/refill`；检查保证金是否充足 |
| 启动后停在 error 状态 | 对账发现仓位偏差超容忍度 | 查看日志中的期望值与实际值对比，人工确认后处理 |
| 时间戳/签名相关拒绝 | 系统时间不准 | 同步 NTP |

### 日志与诊断

```bash
# Linux
journalctl -u gridbot -f                       # 实时
journalctl -u gridbot --since "1 hour ago"     # 最近一小时
grep -i error /var/log/gridbot/gridbot.log
```

```powershell
# Windows
Get-Content C:\gridbot\logs\gridbot.log -Tail 200 -Wait
Select-String -Path C:\gridbot\logs\gridbot.log -Pattern "error" -CaseSensitive:$false
```

排查具体问题时把 `log_level` 临时调成 `debug` 并重启，会输出每笔请求的详细信息。**注意 debug 日志量很大，问题解决后记得调回 `info`。**

完整历史看日志文件。`GET /api/exchanges/{ex}/logs` 读的是内存环形缓冲，只保留最近若干条。

---

## 14. 卸载

**卸载前务必先停止网格并确认交易所侧无残留挂单与仓位。** 停止：`POST /api/exchanges/lighter/stop`，或给进程发 SIGTERM（只撤本交易对挂单、保留仓位）。

### Linux

```bash
sudo systemctl disable --now gridbot
sudo rm /etc/systemd/system/gridbot.service /etc/logrotate.d/gridbot /etc/cron.d/gridbot-backup
sudo systemctl daemon-reload

sudo rm -rf /opt/gridbot /var/log/gridbot
sudo rm -f  /etc/gridbot.env

# 数据（含成交历史）确认不需要后再删
sudo rm -rf /var/lib/gridbot

sudo userdel gridbot
```

### Windows

```powershell
nssm stop   gridbot
nssm remove gridbot confirm
Unregister-ScheduledTask -TaskName gridbot -Confirm:$false -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName gridbot-backup -Confirm:$false -ErrorAction SilentlyContinue

Remove-Item -Recurse -Force C:\gridbot
```

删除后**记得去 Lighter 后台吊销本程序使用的 API Key**，避免密钥残留在备份或历史文件里造成风险。
