# rsync-proxy ![](https://github.com/ustclug/rsync-proxy/workflows/Go/badge.svg)

rsync-proxy 可以根据 module name 反向代理不同 host 上的 rsync daemon 以减轻单台主机上的 IO 压力。

# 1. 安装

根据 OS 到 [release](https://github.com/ustclug/rsync-proxy/releases) 页面里下载相应的 tarball。下载并 cd 到解压出来的目录后：

## 1.1 创建配置文件

```bash
# mkdir /etc/rsync-proxy
# cp ./cofig.example.toml /etc/rsync-proxy/config.toml
# vim /etc/rsync-proxy/config.toml # 根据实际情况修改配置
```

## 1.2 创建 systemd service

```bash
# cp ./rsync-proxy.service /etc/systemd/system/
# cp ./rsync-proxy /usr/local/bin/
# systemctl enable --now rsync-proxy
```

# 2. 配置

```toml
[proxy]
# 可选，设置访问时输出的 motd 内容（默认值为空字符串，即不输出）
motd = "Served by rsync-proxy (https://github.com/ustclug/rsync-proxy)"

# "u1" 表示 upstream 的名字，不能重复
[upstreams.u1]
host = "127.0.0.1"
port = 1234
# 该 upstream 下所有的 module，不能重复（即便是在不同的 upstream）
modules = ["foo"]

[upstreams.u2]
host = "example.com"
port = 1235
modules = ["bar"]
```
