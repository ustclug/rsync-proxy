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

### 安装 fail2ban filter

```shell
cp fail2ban/filter.d/* /etc/fail2ban/filter.d/
```

# 配置

除了基本的监听地址、TLS、access/error log 等，rsync-proxy 还提供了一组连接保护与限流相关的配置项，全部默认关闭、可以按需启用，包括：relay 阶段的 idle 超时与最大持续时长、TCP keepalive 周期、单 IP 对单上游的并发连接数上限、上游拨号超时、relay 阶段的吞吐率下限（带 grace 期，避免误伤大 module 的 file-list 阶段）。各上游还可以独立配置最大并发与排队上限，并支持 PROXY protocol。

完整字段含义、推荐起点值与公共 mirror 的取值依据，见 [`assets/config.example.toml`](assets/config.example.toml)。

# 监控

rsync-proxy 在 `listen_http` 上暴露 Prometheus 格式的 `/metrics` 端点，覆盖连接生命周期、按 module/upstream 的累计流量、排队与失败计数、各类终止原因（idle/max-duration/throughput-floor/per-IP），以及 Go runtime 指标。

仓库 [`grafana/dashboard.json`](grafana/dashboard.json) 提供了一份现成的 Grafana dashboard，对应上述指标。
