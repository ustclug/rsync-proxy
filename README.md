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

注意：由于技术原因，`listen` 和 `listen_http` 在重新载入配置文件时不会更新。如果需要更新这些设置，请重启进程。

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
