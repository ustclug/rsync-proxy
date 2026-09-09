# rsync-proxy ![](https://github.com/ustclug/rsync-proxy/workflows/Go/badge.svg)

rsync-proxy 可以根据 module name 反向代理不同 host 上的 rsync daemon 以减轻单台主机上的 IO 压力。

# 安装

## 使用 deb 包

到 [release](https://github.com/ustclug/rsync-proxy/releases) 页面里下载相应的 deb 包，安装后修改配置文件：

```shell
sudo cp /etc/rsync-proxy/config.example.toml /etc/rsync-proxy/config.toml
vim /etc/rsync-proxy/config.toml  # 根据实际情况修改配置
```

`sudo systemctl start rsync-proxy` 启动即可。如果需要配置 fail2ban，可参考 [fail2ban](assets/fail2ban/)。

## 手动安装

### 编译

```shell
make linux-amd64
# macOS: make darwin-amd64
cd build/rsync-proxy-......  # cd 到编译的目标目录
```

Linux 二进制程序也可从 release 页面下载。

### 创建配置文件

```shell
mkdir /etc/rsync-proxy
cp config.example.toml /etc/rsync-proxy/config.toml
vim /etc/rsync-proxy/config.toml  # 根据实际情况修改配置
```

注意：由于技术原因，`listen`、`listen_tls` 和 `listen_http` 在重新载入配置文件时不会更新。如果需要更新这些设置，请重启进程。

如果配置了 `listen_tls`、`tls_cert_file` 和 `tls_key_file`，rsync-proxy 会额外开启一个 TLS rsync 监听端口，与明文 `listen` 共存。重新载入配置时会自动重读证书和私钥，新连接会立即使用新证书，已有连接不受影响。

### 创建用户与 systemd service

```shell
useradd --system --shell /usr/sbin/nologin rsync-proxy
cp rsync-proxy.service /etc/systemd/system/
sed '|s|/usr/bin/rsync-proxy|/usr/local/bin/rsync-proxy|' -i /etc/systemd/system/rsync-proxy.service
cp rsync-proxy /usr/local/bin/
systemctl enable --now rsync-proxy.service
```

### 使用 logrotate 滚动日志

```shell
cp logrotate.conf /etc/logrotate.d/
```

### 零停机更新（Linux）

运行中的版本具备升级协议后，可以先原子替换安装路径上的二进制，再执行：

```shell
sudo install -m 0755 rsync-proxy.new /usr/bin/rsync-proxy.next
sudo mv /usr/bin/rsync-proxy.next /usr/bin/rsync-proxy
sudo /usr/bin/rsync-proxy --config=/etc/rsync-proxy/config.toml upgrade --timeout=30s
```

`upgrade` 等待新进程就绪并完成监听交接后返回，不等待旧连接结束。已有明文/TLS 传输、握手和排队连接留在原进程；新连接由新进程接收。可以在旧连接未结束时继续升级，形成多代共存。`reload` 仍然只重读配置；`systemctl restart` 是普通重启，不是零停机更新入口。

- `[proxy].state_dir` 默认为 `/run/rsync-proxy`，保存 supervisor 的 Unix socket 和进程锁文件。全局限额、FIFO 队列和各代快照保存在 supervisor 内存中，不使用数据库。手动运行时须使用可写目录，各实例须使用独立目录；不要在进程运行期间删除 socket 或锁文件。
- `[proxy].upgrade_drain_timeout` 单位为秒，默认 `0`。非零值从该代停止接收新连接时起计算，到期关闭剩余连接，包括握手和排队。采用本次成功升级的新配置值；后续更新不会改变或重置更早各代的期限。默认不增加期限，原有 idle/max-duration/throughput 规则继续生效。
- 所有代合计执行上游并发、排队和单 IP 限额，排队按全局 FIFO 晋升。调低限额不会主动断开已有连接；等待占用降到新限额以下再放行。旧连接继续使用已经选定的上游目标。
- 热更新要求 `listen`、`listen_tls`、`listen_http` 和状态目录保持一致。新程序、配置、证书或协议不兼容，或就绪超过 `--timeout`，会报告失败并保留旧进程。准入操作不做磁盘写入，队列变化通过 IPC 通知 worker。
- `connections`、`/status` 和 `/metrics` 汇总各代，逐连接数据新增 `generation` 标识；Go/runtime 指标属于当前接收连接的 worker。跨代快照每秒发布，正常退出前发布最终值，业务累计指标保留已退出代的贡献；异常退出可能损失最后一次未发布的统计。一次完全停止后的启动会重置运行状态。
- 使用配套 systemd unit，主 PID 始终是 supervisor。supervisor 保留监听 FD，通过 Unix socket 协调 worker；`upgrade` 启动安装路径上的新 worker，并让旧 worker 排空连接。普通停止仍受 systemd 的停止超时约束。旧 worker 仍有连接时会继续占用内存和 FD，可通过退出期限限制。
- supervisor 确认 worker 退出后回收其额度；当前 worker 意外退出时，会从安装路径启动替代 worker。supervisor 或控制连接异常失效时，worker 停止接收并关闭连接，不会另建空状态继续放行。
- `upgrade` 更新 worker，不替换常驻 supervisor；修改 supervisor 实现或 IPC 协议后，需要普通重启才能应用这部分更新。不兼容的 worker 会被拒绝，旧 worker 继续服务。
- 日志轮转使用 `rsync-proxy --config=... reopen-logs`，让所有存活的代重新打开日志。升级与日志重开命令读取配置中的管理地址，显式 `--host` 可覆盖。

**首次从不支持 supervisor 协议的旧版（包括 SQLite 版本）迁入需要一次普通重启。** deb 升级会对正在运行的服务调用 `upgrade`，失败不会自动强制重启；首次迁入或不兼容升级时，检查错误后手动 `systemctl restart rsync-proxy`，再完成包配置。进程崩溃、主机重启、主动停止和配置的连接超时不属于零停机保证。

开发验证：`go test -race ./...` 包含真实子进程的多代交接、supervisor IPC 并发调度及故障测试。真实用户级 systemd 验证可运行 `python3 test/systemd/upgrade.py --binary /absolute/path/to/rsync-proxy`；该脚本只创建临时测试 unit、回环监听和临时目录。

### 安装 fail2ban filter

```shell
cp fail2ban/filter.d/* /etc/fail2ban/filter.d/
```

# 配置

配置文件采用 TOML 格式，分为两段：`[proxy]` 段是 rsync-proxy 自身的设置，`[upstreams.<NAME>]` 段为每个上游 rsync daemon 一项。完整示例见 [`assets/config.example.toml`](assets/config.example.toml)。

## `[proxy]` 基础

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `listen` | string | — | 明文 rsync 监听地址，如 `"0.0.0.0:873"`。 |
| `listen_tls` | string | — | TLS rsync 监听地址，可选。设置后会与 `listen` 共存。 |
| `listen_http` | string | — | 控制面 HTTP 监听地址，可为 `host:port` 或 unix socket 路径。`/metrics`（Prometheus）、`/reload`、`/status` 由此暴露。 |
| `tls_cert_file` | string | — | TLS 证书文件，配 `listen_tls` 使用。reload 时自动重读。 |
| `tls_key_file` | string | — | TLS 私钥文件。reload 时自动重读。 |
| `access_log` | string | stdout | 访问日志路径。 |
| `access_json_log` | string | 空 | JSON Lines 访问日志路径；每个 rsync 连接写入一行 JSON，空值表示禁用。 |
| `error_log` | string | stderr | 错误日志路径。 |
| `motd` | string | 空 | 客户端连接成功后展示的一行欢迎信息。 |

注：`listen`、`listen_tls`、`listen_http` 在 reload 时不会更新，需重启进程。

## `[proxy]` 连接保护与限流

下列字段全部默认为 `0`（关闭），按需启用。完整含义、推荐起点值与取舍在 [`assets/config.example.toml`](assets/config.example.toml) 中有详细注释。

| 字段 | 类型 | 默认 | 含义 | 公共 mirror 推荐起点 |
| --- | --- | --- | --- | --- |
| `relay_idle_timeout` | int 秒 | 0 | relay 阶段双向无 I/O 多久后关闭连接。语义同 rsyncd `timeout`。 | `600` |
| `relay_max_duration` | int 秒 | 0 | relay 阶段总时长上限。超时关闭，rsync 客户端通常会重连续传。 | `14400`（4h） |
| `tcp_keepalive` | int 秒 | 0 | 客户端连接和上游连接的 TCP keepalive 周期，0 沿用 OS 默认（通常 ~2h）。 | `120` |
| `per_ip_max_active_connections` | int | 0 | 单 IP 对单上游最多同时持有的连接数（含排队），proxy-wide 默认值。NAT/校园出口 IP 时取值需放宽。 | `4` |
| `dial_timeout` | int 秒 | 0 | 拨号上游的超时；0 沿用内核 SYN 重试（~75s）。 | `5`（LAN） |
| `min_throughput_bytes` | int64 字节 | 0 | relay 阶段最近 `min_throughput_window` 秒内累计收发须 ≥ 此值，否则视作慢速吸血并关闭。0 关闭整组检查。 | `1048576` |
| `min_throughput_window` | int 秒 | 60 | 上述滑动窗口长度。 | `60` |
| `min_throughput_grace` | int 秒 | = window | 连接刚开始的豁免期，避免误杀大 module 的 file-list 阶段。 | `600` |

各项触发的事件都有对应的 Prometheus counter（`rsync_proxy_relay_idle_timeout_terminated_total`、`rsync_proxy_relay_max_duration_terminated_total`、`rsync_proxy_throughput_floor_terminated_total`、`rsync_proxy_per_ip_rejected_total`、`rsync_proxy_upstream_dial_errors_total`），便于先观察再调参。

## `[upstreams.<NAME>]`

每个上游一项，名字（`NAME`）任意，仅用于日志与 metric 标签。

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `address` | string | — | 上游地址，可为 `host:port` 或 unix socket 路径（如 `/run/rsyncd.sock`，需上游通过 xinetd 或 `systemd.socket Accept=yes` 暴露）。 |
| `modules` | []string | — | 该上游提供的 module 列表。多个上游声明相同 module 时按客户端 IP 做负载均衡。 |
| `discover_modules` | bool | false | 启动/reload 时自动从上游拉 module 列表。上游不可达会导致启动失败。与 `modules` 二选一。 |
| `use_proxy_protocol` | bool | false | 与上游通信时附加 PROXY protocol 头，便于上游记录真实客户端 IP。需上游 rsyncd 启用 PROXY protocol 支持。 |
| `max_active_connections` | int | 0 | 该上游最大并发 relay 连接数，0 表示不限制。 |
| `max_queued_connections` | int | 0 | 达到上限后排队的最大长度，0 表示不限排队长度。 |
| `per_ip_max_active_connections` | int | 0 | 覆盖 `[proxy]` 中的同名值；0 表示继承 proxy-wide 默认。 |

# 监控

rsync-proxy 在 `listen_http` 上暴露 Prometheus 格式的 `/metrics` 端点，覆盖连接生命周期、按 module/upstream 的累计流量、排队与失败计数、各类终止原因（idle/max-duration/throughput-floor/per-IP），以及 Go runtime 指标。

仓库 [`contrib/grafana/dashboard.json`](contrib/grafana/dashboard.json) 提供了一份现成的 Grafana dashboard，对应上述指标。
