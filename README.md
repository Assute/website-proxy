# website-proxy

一个网站中转加速服务，用来把目标网站通过当前服务器地址进行访问。

仓库里的 `website-proxy` 文件是给 Alpine Linux 使用的二进制。

它的主要作用是：

- 给目标网站提供一个统一的中转加速入口
- 隐藏目标网站的真实访问地址
- 让页面、静态资源和部分接口继续走当前中转地址
- 通过当前服务器转发访问请求，优化访问体验

如果你只关心怎么运行，直接看下面就够了。

## 需要的文件

把这 3 个文件放到 Alpine 服务器：

- `/opt/website-proxy/website-proxy` 这是 Alpine Linux 二进制
- `/opt/website-proxy/website-proxy.initd`
- `/opt/website-proxy/config.json`

## 怎么运行

首次安装执行：

```sh
mkdir -p /opt/website-proxy
chmod +x /opt/website-proxy/website-proxy
cp /opt/website-proxy/website-proxy.initd /etc/init.d/website-proxy
chmod +x /etc/init.d/website-proxy
rm -f /run/website-proxy.pid
rc-update add website-proxy default
rc-service website-proxy start
rc-service website-proxy status
tail -n 50 /var/log/website-proxy.log
```

## 配置文件

程序会自动读取：

```text
/opt/website-proxy/config.json
```

示例：

```json
{
  "target_url": "https://example.com",
  "allowed_host_suffixes": [
    "example.com"
  ],
  "port": 16800,
  "upstream_proxy_on": false,
  "upstream_proxy_url": "",
  "access_log": false
}
```

改完 `config.json` 后重启：

```sh
rc-service website-proxy restart
tail -n 50 /var/log/website-proxy.log
```

## 常用命令

启动：

```sh
rc-service website-proxy start
```

停止：

```sh
rc-service website-proxy stop
```

重启：

```sh
rc-service website-proxy restart
```

查看状态：

```sh
rc-service website-proxy status
```

查看日志：

```sh
tail -f /var/log/website-proxy.log
```

## 更新程序

替换新的二进制后执行：

```sh
rc-service website-proxy stop || true
rm -f /run/website-proxy.pid
chmod +x /opt/website-proxy/website-proxy
rc-service website-proxy start
tail -n 50 /var/log/website-proxy.log
```

## 访问入口

默认入口：

```text
http://你的服务器IP:16800/go
```

## 说明

- 根路径 `/` 默认返回 `404`
- 如果日志里提示端口被占用，先停掉旧进程再启动
- 如果你改了 `config.json`，重启服务后才会生效
