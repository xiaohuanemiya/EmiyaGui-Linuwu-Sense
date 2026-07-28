# 从零安装：Ubuntu Server 26.04 → 可用的 Web 控制面板

面向 Acer Predator PHN16-71（Ubuntu 26.04 "resolute"，内核 `7.0.0-28-generic`，
Secure Boot 关闭）。其它 Predator / Nitro 机型思路相同，但 DMI 与 quirk 需要自行确认。

> 2026-07-28 在一台刚装完 Ubuntu Server 26.04 的 PHN16-71 上**端到端完整跑过一遍**，
> 期间发现并修复了两个只在干净系统上才会暴露的问题（见文末「验证记录」）。

[README.md](README.md) 讲的是**部署方式的选择**（系统级 systemd / 用户级 / Podman）。
本文讲的是**从装完系统到能用**的完整顺序，重点在驱动这一层和几个会静默出错的坑。

---

## 0. 前提

- 已装好 Ubuntu Server 26.04，能 SSH 登录。装机时记得勾上 **Install OpenSSH server**，
  并用 "Import SSH identity" 导入公钥（或至少设好密码）——后面全程靠 SSH
- **SSH 必须可达**。这个机型（MUX 为独显直连）没有核显，装 NVIDIA 驱动失败会直接黑屏，
  SSH 是唯一的补救通道
- 重装后本机的 SSH 主机密钥会变，客户端要先清掉旧记录才连得上：
  `ssh-keygen -R <本机IP>`
- 确认 Secure Boot 状态：

  ```bash
  mokutil --sb-state
  ```

  显示 `SecureBoot disabled` 就无需签名。若是 enabled，需要 MOK 签名，见驱动仓库的
  `module_signing_readme`，本文不覆盖。

---

## 1. 装依赖

```bash
sudo apt update
sudo apt install -y build-essential dkms git curl openssl linux-headers-$(uname -r)
```

`curl` 和 `openssl` 是后面 `scripts/install-user.sh` 要用的。

---

## 2. NVIDIA 驱动

先装显卡驱动，再装 Linuwu-Sense —— **顺序是有意的**。NVIDIA 装包会触发
`update-initramfs`，如果此时 Linuwu-Sense 已经装好，它的旧构建会被打进 initrd；而
initrd 里那份会在开机约 1 秒时抢先加载，让后续的 `modprobe` 变成空操作。把
Linuwu-Sense 放在最后（它自己会刷新 initramfs）就不会有这种交叉污染。

> 严格说 Web 面板**不依赖**它:GPU 温度取自 acer EC 的 `temp2`，实测与 `nvidia-smi`
> 差异在 1°C 以内。装它是为了 CUDA、游戏和 GPU 占用率这些面板之外的用途。

```bash
sudo ubuntu-drivers install     # 本机推荐并实测的是 nvidia-driver-595-open
```

`ubuntu-drivers` 会优先选预编译已签名的 `linux-modules-nvidia-*`，不走 DKMS，所以
`dkms status` 里看不到 nvidia 是正常的，不代表装失败了。

重启前**再次确认 SSH 可达**——这个机型没有核显，模块加载失败就是黑屏，SSH 是唯一退路。

```bash
sudo reboot
```

回来后确认：

```bash
nvidia-smi --query-gpu=name,driver_version,temperature.gpu --format=csv
```

出不来数就回滚，然后再重启：

```bash
sudo apt remove --purge '^nvidia-.*' && sudo update-initramfs -u
```

---

## 3. 装驱动（Linuwu-Sense fork）

```bash
git clone https://github.com/xiaohuanemiya/Linuwu-Sense.git
cd Linuwu-Sense
git checkout phn16-71
make dkms-install
```

`dkms-install` 会一次做完：注册进 DKMS 并编译、把 `acer_wmi` 加黑名单、写
`modules-load.d` 开机加载、刷新 initramfs、建 `linuwu_sense` 组并把当前用户加进去、
用 tmpfiles 把可写 sysfs 节点设成 `0660 root:linuwu_sense`，最后才装并启动
`linuwu_sense.service`。

> 装之前系统里**本来就有**一个名为 `acer` 的 hwmon —— 那是内核自带的 `acer_wmi`
> （mainline 从 6.10 起就带 hwmon）。它只有 `temp*_input` / `fan*_input`，没有标签，
> 也没有 `predator_sense` 那一套控制节点。本驱动会把 `acer_wmi` 列入黑名单并取而代之，
> 所以安装前后 hwmon 编号可能变化，这也是程序一律按 `name` 枚举的原因之一。

> **刷新 initramfs 这步不能省。** initramfs 里会带一份自己的 `linuwu_sense.ko`，开机约
> 1 秒时就加载，早于 `systemd-modules-load`，导致后面的 `modprobe` 变成空操作。而
> `dkms(8)` 手动安装时**从不**碰 initramfs（在它脚本里 grep `update-initramfs` 是 0 处
> 命中）。Makefile 已经替你补上了这一步；如果你绕过 Makefile 直接用 `dkms install`，
> 必须自己补 `sudo update-initramfs -u`。

### 重新登录

`usermod -aG linuwu_sense` 要新会话才生效。**退出 SSH 再连一次**，然后确认：

```bash
id -nG | tr ' ' '\n' | grep -x linuwu_sense
```

没输出就是还没生效，下一步的安装脚本会直接拒绝运行。

### 验证驱动（别跳过）

```bash
echo "loaded = $(cat /sys/module/linuwu_sense/srcversion)"
echo "ondisk = $(modinfo -F srcversion linuwu_sense)"
```

**两个值必须一致。** 不一致说明内核里跑的是另一个构建（通常就是 initramfs 里那份旧的），
执行 `sudo update-initramfs -u` 后重启再看。

> `dkms status` 显示 `installed` **不能**作为"打了补丁的模块正在运行"的证据 —— 它只说明
> DKMS 把文件装到位了，不代表内核加载的是它。只认 srcversion。

再看传感器通道有没有正确带上标签：

```bash
for h in /sys/class/hwmon/hwmon*; do
  [ "$(cat $h/name 2>/dev/null)" = acer ] || continue
  grep -H . $h/temp*_label $h/fan*_label
  grep -H . $h/temp*_input $h/fan*_input
done
```

应当看到 `CPU` / `GPU` / `System` 和 `CPU Fan` / `GPU Fan`，且都有读数。

---

## 4. 编译 phnctl

宿主机不需要装 Go，用容器编译即可：

```bash
sudo apt install -y podman
git clone <本仓库地址> EmiyaGui-Linuwu-Sense
cd EmiyaGui-Linuwu-Sense
podman run --rm -v .:/src:Z -w /src docker.io/library/golang:1.24-alpine \
  sh -c 'go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /src/phnctl ./cmd/phnctl'
install -D -m 0755 phnctl ~/.local/bin/phnctl
```

宿主机装了 Go 的话，直接 `go build` 同样可以。项目没有任何第三方依赖。

---

## 5. 安装为用户级服务

```bash
PHNCTL_INSTALL_BIND=<本机IP>:8443 scripts/install-user.sh deploy/phnctl-user.service
```

> `PHNCTL_INSTALL_BIND` 不传的话默认是 `192.168.1.239:8443`（作者机器的地址），**换机器
> 一定要显式传**。

脚本会生成 TLS 自签证书和会话密钥，写好用户 unit 并启动。**它不会生成密码** —— 密码在
下一步由你在浏览器里自己设。

脚本结束时会打印一个一次性**安装令牌**，形如：

```
Open it in a browser and set your own admin password. It will ask for the
one-time setup token below, which exists only until a password is set:

    kJ3nQ8...
```

令牌同时写在 `~/.config/phnctl/setup-token.txt`（`0600`），也可以从
`journalctl --user -u phnctl` 里取。

其它部署形态（系统级 systemd、rootless Podman）见 [README.md](README.md)。

---

## 5b. 首次打开：设置管理员密码

浏览器访问 `https://<本机IP>:8443`，会直接进入设置表单（而不是登录框）。填入上一步的
安装令牌、用户名和你自己的密码（**至少 12 位**），提交后即自动登录。

- 令牌在设置成功的瞬间作废，重放会被拒（HTTP 409）
- 密码哈希写入 `~/.config/phnctl/credentials`（`0600`），重启后沿用
- 设置完成前，所有接口都返回 `setup_required`，面板不可用

> 令牌存在的理由：密码设好之前，设置接口不可能要求认证，谁先连上谁就能占用管理员账户。
> 令牌把这个窗口关掉。设置完成后 `setup-token.txt` 就没用了，可以删掉。

---

## 6. 开启 linger（服务器上必做）

用户级服务默认绑定在用户会话上：**注销就停，重启后没人登录就永远不启动**。Server 装法
没有图形会话，这一条是致命的。

```bash
sudo loginctl enable-linger "$USER"
loginctl show-user "$USER" -p Linger      # 应输出 Linger=yes
```

本机实测：开启后重启，phnctl 在**第一个登录会话出现之前 261 秒**就已在监听。

---

## 7. 让浏览器信任证书

先核对指纹，再决定是否导入系统信任库（导入是你自己的操作）：

```bash
PHNCTL_TLS_CERT=$HOME/.config/phnctl/tls/cert.pem ~/.local/bin/phnctl cert
```

输出含 subject、SAN、有效期和 SHA-256 指纹。不导入也能用，只是浏览器每次告警，且日志里
会持续出现 `TLS handshake error`（程序已限流到每 5 分钟一条）。

---

## 8. 收尾验证

```bash
# 驱动：两个值一致
echo "loaded = $(cat /sys/module/linuwu_sense/srcversion)"
echo "ondisk = $(modinfo -F srcversion linuwu_sense)"

# 权限：0660 root:linuwu_sense
ls -l /sys/firmware/acpi/platform_profile

# 服务
systemctl --user is-active phnctl
loginctl show-user "$USER" -p Linger

# 面板可达
curl -kI https://<本机IP>:8443/
```

浏览器打开 `https://<本机IP>:8443`，用 §5b 里自己设的账号密码登录。仪表盘应显示 CPU/GPU
温度与双风扇转速；顶部若出现黄色提示条，说明某些硬件项读取失败，条目名即失败的部分，
其余数据仍然有效。

**重启一次再复查上面这组命令** —— 本项目遇到过的问题里，有两个只在重启后才暴露。

---

## 常见坑

| 现象 | 原因 | 处理 |
|---|---|---|
| `loaded` 与 `ondisk` 的 srcversion 不一致 | initramfs 里的旧模块开机抢先加载 | `sudo update-initramfs -u` 后重启 |
| `temp*_label` 不存在 | 同上，跑的是没有 label 的旧构建 | 同上 |
| GPU 温度显示 0 / 传感器不可用 | 旧版本依赖 `nvidia-smi`；nouveau 对 Ada 不提供 hwmon | 已修复：GPU 温度回退到 acer EC 通道 |
| `install-user.sh` 报不在 `linuwu_sense` 组 | `usermod -aG` 需要新会话 | 退出 SSH 重连 |
| 注销后面板打不开 / 重启后打不开 | linger 未开 | `sudo loginctl enable-linger "$USER"` |
| 面板整体不可用而非单项缺失 | 快照读取整体失败 | 看 `journalctl --user -u phnctl`，找 `degraded hardware sections` |
| `dkms status` 正常但功能不对 | 它只证明文件装好了 | 一律以 srcversion 为准 |
| hwmon 编号变了导致读不到 | `hwmonN` 不稳定（换 GPU 驱动后本机从 `hwmon4` 变 `hwmon3`） | 程序按 `name` 枚举，不受影响；自己写脚本时别硬编码 |

---

## 验证记录

2026-07-28，在一台刚装完 Ubuntu Server 26.04（内核 `7.0.0-28-generic`，Secure Boot
关闭，`/` 与 `/home` 全新格式化）的 PHN16-71 上按本文顺序完整走了一遍。过程中发现两个
**只在干净系统上才会暴露**的问题，均已修复并推送：

**1. `configure` 里 systemd unit 的安装顺序错误**（`3d44f59`）

`linuwu_sense.service` 在 `/etc/tmpfiles.d/linuwu_sense.conf` 生成**之前**就被
enable + start，而该 unit 的 `ExecStart` 正是 `systemd-tmpfiles --create` 这个文件；
文件不存在时 `systemd-tmpfiles` 退出码为 1 → 服务启动失败 → `configure` 返回非零 →
`dkms-install` 整体中断，导致 `linuwu_sense` 组和 tmpfiles 规则**都没建起来**。

之前一直没暴露，是因为老机器上那个 conf 文件是历史遗留、早就存在。修复方式是把 unit
的安装挪到 configure 末尾。

**2. label 补丁不在 `phn16-71` 分支上**（`29cdec8`）

hwmon 标签那个 commit 当初提交在 `claude/gpu-sensor-fix-f79230`（基于 `main`），而本文
让你 checkout 的是 `phn16-71`，两个分支从未合并。照文档装完会**没有任何 label**，而
§4 的验证步骤又要求检查 label —— 必然失败。已 cherry-pick 到 `phn16-71`。

### 通过的项目

| 项目 | 结果 |
|---|---|
| DMI 匹配 | `Acer` / `Predator PHN16-71`，与 quirk 一致 |
| NVIDIA | `nvidia-driver-595-open` 595.84，预编译模块，重启后 `nvidia-smi` 正常 |
| 驱动 srcversion | 重启后 loaded == ondisk，**initramfs 陈旧模块问题未复现** |
| 五个 label | 干净重启后自然出现，无需手动 `rmmod`/`modprobe` |
| 温度交叉校验 | acer `temp2` 36°C vs `nvidia-smi` 36°C |
| sysfs 权限 | `0660 root:linuwu_sense`，重启后保持 |
| phnctl 构建 | 容器内 `gofmt`/`vet` 干净，全部测试通过 |
| 首次设置接口 | 未设置时 `setupRequired:true`；受保护接口 403 `setup_required`；错误令牌 401 |
| linger | `Linger=yes`，服务 active/enabled |

### 一处未能有效测量

重启后 phnctl 启动于 +128s，而首次 SSH 登录也记录在 +128s，**无法据此证明先后顺序** ——
因为验证时在轮询 SSH，把登录时间压到了同一秒。linger 生效的有效证据来自另一次干净测量
（phnctl 早于首个会话 261 秒）。
