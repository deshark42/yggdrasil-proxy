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

服务器通过 `authlib-injector` 将认证请求指向本代理。代理默认按配置的优先级依次尝试上游 Yggdrasil 服务，并缓存玩家上次认证成功的上游。后续请求优先向缓存上游重新验证，未成功时再按优先级尝试其他上游；同一次请求不会重复尝试相同的上游 URL。缓存只记录上游关联，不缓存或复用认证结果。

`hasJoined` 的结果分为三类：

- **成功**：上游返回 `200`，响应是有效的角色 JSON，包含 32 位十六进制 UUID（`id`）和非空角色名（`name`）。代理原样返回响应内容，并更新玩家的上游缓存。响应体最大为 1 MiB，超过限制视为上游错误。
- **未通过验证**：上游返回 `204`，继续尝试其他上游，不因本次验证失败删除缓存。
- **上游错误**：网络故障、超时、其他 HTTP 状态码、响应读取失败或无效角色资料。记录错误并继续尝试其他上游，不因此删除缓存。

任一上游成功时返回 `200`；全部上游均返回 `204` 时返回 `204`；没有成功且至少一个上游发生错误时返回 `502 Bad Gateway`。这与旧版本将所有失败统一返回 `204` 的行为不同。下游请求取消后停止尝试，不更新缓存。请求中的可选 `ip` 参数会转发给上游。

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
