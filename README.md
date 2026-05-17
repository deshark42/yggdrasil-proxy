# Yggdrasil Proxy

Yggdrasil 认证代理 — 让 Minecraft 服务器同时支持 **Mojang 官方登录** 和 **第三方皮肤站认证**（如 LittleSkin）。

## 工作原理

```
Minecraft Server (authlib-injector)
        │
        ▼
Yggdrasil Proxy (本工具)
        │
        ├── 按优先级 → Mojang Session Server
        └── 按优先级 → LittleSkin / 其他第三方
```

服务器通过 `authlib-injector` 将认证请求指向本代理，代理按配置的优先级依次尝试上游 Yggdrasil 服务，并将首次成功的服务器缓存，后续同一玩家的请求直接复用。

## 快速开始

### 1. 下载

~~从 [Releases](https://github.com/deshark42/yggdrasil-proxy/releases) 下载对应平台的二进制文件，或~~使用 Docker：

```bash
docker pull ghcr.io/deshark42/yggdrasil-proxy:latest
```

### 2. 配置

创建 `config.json`：

```json
{
    "port": 32217,
    "authServers": [
        {
            "priority": 0,
            "name": "Mojang",
            "url": "https://sessionserver.mojang.com",
            "timeout": 5
        },
        {
            "priority": 1,
            "name": "LittleSkin",
            "url": "https://littleskin.cn/api/yggdrasil/sessionserver",
            "timeout": 5
        }
    ]
}
```

配置文件不存在时，首次启动会自动生成上述默认配置。

### 3. 启动

**直接运行：**
```bash
./yggdrasil-proxy
```

**Docker：**
```bash
docker run -d -p 32217:32217 -v ./config.json:/app/config.json ghcr.io/deshark42/yggdrasil-proxy:latest
```

**Docker Compose：**
```yaml
services:
  yggdrasil-proxy:
    image: ghcr.io/deshark42/yggdrasil-proxy:latest
    ports:
      - "32217:32217"
    volumes:
      - ./config.json:/app/config.json
```

### 4. 连接 Minecraft 服务器

在服务器 JVM 参数中添加：

```
-javaagent:authlib-injector-1.x.x.jar=http://你的代理地址:32217
```

## 配置说明

| 字段 | 说明 |
|------|------|
| `port` | 监听端口，默认 `32217` |
| `authServers` | 认证服务器列表，按 `priority` 从小到大依次尝试 |
| `priority` | 优先级，数值越小越优先 |
| `name` | 标识名，仅用于日志 |
| `url` | Yggdrasil 认证服务地址 |
| `timeout` | 请求超时时间（秒） |

## API

| 路径 | 说明 |
|------|------|
| `GET /` | 返回代理元数据 |
| `GET /sessionserver/session/minecraft/hasJoined?username=<玩家>&serverId=<id>` | 玩家认证代理 |

## 从源码构建

```bash
git clone https://github.com/deshark42/yggdrasil-proxy.git
cd yggdrasil-proxy
go build -o yggdrasil-proxy .
```

---

镜像地址：`ghcr.io/deshark42/yggdrasil-proxy`
