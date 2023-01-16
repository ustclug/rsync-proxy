# rsync-proxy ![](https://github.com/ustclug/rsync-proxy/workflows/Go/badge.svg)

rsync-proxy 可以根据 module name 反向代理不同 host 上的 rsync daemon 以减轻单台主机上的 IO 压力。

# 安装

根据 OS 到 [release](https://github.com/ustclug/rsync-proxy/releases) 页面里下载相应的 tarball。下载并 cd 到解压出来的目录后：

## 创建配置文件

```shell
mkdir /etc/rsync-proxy
cp cofig.example.toml /etc/rsync-proxy/config.toml
vim /etc/rsync-proxy/config.toml  # 根据实际情况修改配置
```

注意：由于技术原因，`listen` 和 `listen_http` 在重新载入配置文件时不会更新。如果需要更新这些设置，请重启进程。

## 创建 systemd service

```shell
cp rsync-proxy.service /etc/systemd/system/
cp rsync-proxy /usr/local/bin/
systemctl enable --now rsync-proxy.service
```
