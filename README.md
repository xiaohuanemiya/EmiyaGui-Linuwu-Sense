# Predator Control

面向 Acer Predator PHN16-71 家庭服务器的轻量 Web 控制面板。它直接读写
`linuwu_sense` 与标准 Linux sysfs，不依赖 DAMX Python daemon；Go 二进制内嵌完整前端，
部署后只有一个服务进程。

从零开始的完整安装顺序（装完 Ubuntu Server 26.04 之后，含驱动层）见
[INSTALL.md](INSTALL.md)。本文侧重功能说明与部署方式的选择。

## 已实现

- Eco / Silent / Balanced / Performance / Turbo 模式映射，并按交流电或电池供电实时过滤。
- Auto / Manual / Max 风扇控制、CPU/GPU 转速和温度历史；Silent 模式下锁定手动风扇。
- 电池保护、校准、键盘背光超时、LCD 低延迟、关机 USB 充电和开机音效。
- 四区静态 RGB 与 8 种动态效果；固件忽略的参数会在界面中停用。
- 启动时按实际节点探测能力，缺失功能不会显示。
- WebSocket 每 2 秒推送遥测；写入后立即回读真实硬件状态。
- 遥测只在有浏览器连接时轮询；温度/转速每 2 秒读，设置类节点缓存 30 秒（写入后立即失效）。
  传感器路径解析一次后缓存，读失败才重新枚举——这三项把常驻 CPU 从 3~4% 降到测不出来。
- 单个属性读失败只标记该项（`state.degraded`），界面顶部提示，其余数据照常显示。
- TLS、登录会话、同源写保护、登录限流、全局 EC 写限速，以及危险操作确认。

## 实机核验结果

核验主机：`emiya-ubuntu`，Ubuntu 26.04，kernel `7.0.0-28-generic`。

- `coretemp/temp1` 的标签是 `Package id 0`，可作为 CPU 温度。
- **GPU 温度取自 `acer` hwmon 的 GPU 通道。** NVIDIA 专有驱动不提供任何 hwmon 节点
  （`nvidia-smi` 是它唯一的接口），所以内核里能拿到的 GPU 读数只有 EC 这一个。实测
  与 `nvidia-smi` 的差异在 1°C 以内（空载 35 vs 36，12/12 次采样）。若存在独立的
  `nvidia` / `amdgpu` hwmon，仍优先使用它。
- 传感器通道按 `*_label` 匹配（`CPU` / `GPU` / `System`、`CPU Fan` / `GPU Fan`）。
  未打 label 补丁的驱动则回退到既定通道顺序 `temp1`/`temp2`、`fan1`/`fan2`。
- 所有 hwmon 一律按 `name` 枚举，不依赖 `hwmonN` 编号 —— 该编号并不稳定：把 GPU 驱动
  从 nouveau 换成 NVIDIA 专有驱动后，`acer` 从 `hwmon4` 变成了 `hwmon3`，DRM 节点也
  从 `card0` 变成了 `card1`。
- 驱动的电池校准接口只提供一个启停位，没有进度或完成原因；界面对启动操作强制确认。
- rootless Podman 以 `--userns=keep-id --group-add keep-groups` 运行时，已成功在容器内将
  当前安全风扇值 `0,0` 原样写回并读出。容器把宿主 sysfs 映射到 `/host/sys`，避免
  Podman 对容器内特殊 `/sys/firmware` 挂载的屏蔽。
- 最终 scratch 镜像约 6.5 MB；实机运行时 Podman 记录的内存快照约 7.7 MB，低于
  20 MB RSS 目标。
- 实机用 `50,50` 分别验证 `quiet`、`balanced`、`balanced-performance` 和
  `performance`；四种模式都成功回读 `50,50`，风扇约为 3648–3802 RPM。检查结束后已
  自动恢复 `balanced` 与 `0,0`。界面仍按 DAMX 规则在 Silent 模式禁用手动控制。

因此项目提供已验证的 Containerfile；直接 systemd 仍是默认部署路径，故障面更小。

## 构建与测试

项目没有第三方 Go 依赖。主机未安装 Go 时，可直接使用 Podman：

```bash
mkdir -p bin
podman run --rm \
  -v "$PWD:/src:ro" -v "$PWD/bin:/out:rw" -w /src \
  docker.io/library/golang:1.24-alpine \
  sh -c 'go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/phnctl ./cmd/phnctl'
```

也可以构建容器镜像；镜像构建过程会先执行全部测试：

```bash
podman build --build-arg VERSION=1.0.0 -t localhost/phnctl:1.0.0 .
```

核心 codec 测试覆盖 `cpu,gpu`、四区 RGB 和
`mode,speed,brightness,direction,r,g,b` 的正常值、边界值与错误值。sysfs 控制器依赖
可注入的文件系统接口，测试只使用内存 fixture，不接触真实硬件。

仓库中的 `scripts/verify-hardware.sh` 是可选实机诊断：它只用 50% 的安全风扇值短暂检查
各交流电模式，且用 trap 恢复原始性能档和风扇设置。脚本必须显式传入
`--accept-hardware-writes` 才会运行。

## 推荐部署：直接 systemd

以下安装命令会正常请求 sudo 密码；运行中的服务本身没有 sudo 权限，也不以 root 运行。

```bash
sudo useradd --system --home-dir /var/lib/phnctl --create-home --shell /usr/sbin/nologin phnctl
sudo install -o root -g root -m 0755 bin/phnctl /usr/local/bin/phnctl
sudo install -d -o root -g phnctl -m 0750 /etc/phnctl/tls
sudo install -o root -g root -m 0644 deploy/phnctl.service /etc/systemd/system/phnctl.service
```

生成登录密码哈希、会话密钥和带本机 IP/DNS 的自签 TLS 证书：

```bash
read -rsp 'Panel password: ' PANEL_PASSWORD; echo
printf '%s' "$PANEL_PASSWORD" | /usr/local/bin/phnctl hash-password
unset PANEL_PASSWORD
openssl rand -base64 32

sudo openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 825 \
  -keyout /etc/phnctl/tls/key.pem \
  -out /etc/phnctl/tls/cert.pem \
  -subj '/CN=emiya-ubuntu' \
  -addext 'subjectAltName=DNS:emiya-ubuntu,IP:192.168.1.239'
sudo chown root:phnctl /etc/phnctl/tls/key.pem /etc/phnctl/tls/cert.pem
sudo chmod 0640 /etc/phnctl/tls/key.pem
```

复制 [deploy/phnctl.env.example](deploy/phnctl.env.example) 为
`/etc/phnctl/phnctl.env`，把上两条命令的输出分别填入
`PHNCTL_PASSWORD_HASH` 和 `PHNCTL_SESSION_SECRET`。然后：

```bash
sudo chown root:phnctl /etc/phnctl/phnctl.env
sudo chmod 0640 /etc/phnctl/phnctl.env
sudo systemctl daemon-reload
sudo systemctl enable --now phnctl.service
systemctl status phnctl.service
```

服务单元明确使用 `User=phnctl`、`SupplementaryGroups=linuwu_sense`，并用 systemd
sandbox 把可写 sysfs 范围限制到目标节点。浏览器访问
`https://192.168.1.239:8443`；首次使用自签证书时需要在浏览器中确认信任。

默认只监听 `192.168.1.239`。若地址发生变化，修改 `PHNCTL_BIND`。只有明确需要监听所有
接口时，才同时设置：

```ini
PHNCTL_BIND=0.0.0.0:8443
PHNCTL_ALLOW_ALL_INTERFACES=true
```

TLS 默认不可关闭。仅为本机回环开发提供
`PHNCTL_INSECURE_HTTP=true`，程序会拒绝在非 loopback 地址上启动不安全 HTTP。

### 无 root 安装

也可把二进制安装到 `~/.local/bin/phnctl`，然后运行：

```bash
scripts/install-user.sh deploy/phnctl-user.service
```

脚本会在 `~/.config/phnctl` 生成 TLS 证书、会话密钥和用户级 systemd 服务，启动后前后端
均由笔记本上的同一进程提供，可通过 `https://192.168.1.239:8443` 访问。

**密码由你在浏览器里自己设置。** 脚本不再生成随机初始密码。首次打开面板时会出现设置
表单，要求填入一次性**安装令牌**，然后由你输入自定的密码（至少 12 位）。令牌在脚本结束
时打印，同时写入 `~/.config/phnctl/setup-token.txt`（`0600`），也可以从
`journalctl --user -u phnctl` 里取。

令牌的作用是防止面板被抢注 —— 在密码设好之前，设置接口无法要求认证，谁先连上谁就能
占用管理员账户。设置完成后令牌立即作废，密码哈希写入 `~/.config/phnctl/credentials`
（`0600`），重启后沿用。

想跳过这个流程、直接指定密码的话，仍然可以自己生成哈希并设 `PHNCTL_PASSWORD_HASH`：
环境变量优先级高于凭据文件，设了就不会进入首次设置模式。

若 `loginctl show-user "$USER" -p Linger` 显示 `Linger=no`，需要执行一次：

```bash
sudo loginctl enable-linger "$USER"
```

这条命令会要求正常的 sudo 密码，并让用户服务在无人登录时及重启后保持运行。

## 可选部署：rootless Podman

实机已验证容器可以写目标 sysfs。下面仍把整个 `/sys` 设为只读，只为三个明确写路径叠加
读写挂载：

```bash
podman run --rm --name phnctl \
  --network host \
  --userns=keep-id --group-add keep-groups \
  --read-only --security-opt=no-new-privileges \
  --env-file /etc/phnctl/phnctl.env \
  --env PHNCTL_SYSFS_ROOT=/host \
  --mount type=bind,src=/etc/phnctl,dst=/etc/phnctl,ro \
  --mount type=bind,src=/sys,dst=/host/sys,ro \
  --mount type=bind,src=/sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/predator_sense,dst=/host/sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/predator_sense,rw \
  --mount type=bind,src=/sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/four_zoned_kb,dst=/host/sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/four_zoned_kb,rw \
  --mount type=bind,src=/sys/firmware/acpi/platform_profile,dst=/host/sys/firmware/acpi/platform_profile,rw \
  localhost/phnctl:1.0.0
```

必须以 `linuwu_sense` 组成员的新登录会话运行。`--group-add keep-groups` 不可省略，也不要
改为 privileged 容器。

## 配置项

| 变量 | 默认值 | 说明 |
|---|---:|---|
| `PHNCTL_BIND` | `192.168.1.239:8443` | 监听地址 |
| `PHNCTL_USERNAME` | `admin` | 登录用户名 |
| `PHNCTL_PASSWORD_HASH` | 无 | 必填，PBKDF2-SHA256 哈希 |
| `PHNCTL_SESSION_SECRET` | 无 | 必填，至少 32 随机字节的 Base64 |
| `PHNCTL_TLS_CERT` / `PHNCTL_TLS_KEY` | 无 | 必填，证书与私钥 |
| `PHNCTL_TELEMETRY_SECONDS` | `2` | 1–30 秒 |
| `PHNCTL_SYSFS_ROOT` | `/` | 测试时替换 sysfs 根目录 |

## 运维与回滚

日志只记录服务和硬件读取错误，不记录密码、会话或请求内容：

```bash
journalctl -u phnctl.service -f
```

传感器故障只在状态变化时打印一次（`degraded hardware sections: ...`），恢复后打印
`all hardware sections readable again`，不会每 2 秒刷屏。浏览器不信任自签证书时产生的
`TLS handshake error` 每 5 分钟最多记录一条，并附带被抑制的条数。

### 让浏览器信任自签证书

先核对指纹，再决定是否导入系统信任库（导入是你自己的操作，本程序不代劳）：

```bash
PHNCTL_TLS_CERT=$HOME/.config/phnctl/tls/cert.pem phnctl cert
```

输出包含 subject、SAN、有效期和 SHA-256 指纹。Windows 上把 `cert.pem` 导入
「受信任的根证书颁发机构」后，用 `https://emiya-ubuntu:8443` 或
`https://192.168.1.239:8443` 访问即可，两者都在 SAN 里。

回滚不会触碰内核模块或 DAMX：

```bash
sudo systemctl disable --now phnctl.service
sudo rm /etc/systemd/system/phnctl.service /usr/local/bin/phnctl
sudo systemctl daemon-reload
```

若确认不再需要配置，可另行备份后删除 `/etc/phnctl`。本项目不修改
`linuwu_sense` 驱动、DKMS 配置或其 tmpfiles 规则。
