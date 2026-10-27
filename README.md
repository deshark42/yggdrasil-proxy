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

缓存默认最多保存 10,000 条关联，每条关联自最近一次成功认证起有效 24 小时；只有认证成功才刷新有效期，读取缓存或认证失败不会续期。每 10 分钟后台清理过期记录，查询和写入时也会清理相应过期记录。达到容量上限时，淘汰最久没有成功认证的关联。过期或淘汰只影响上游尝试顺序，不影响认证判定。

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

参考 [配置示例](<config.example.json>) 创建 `config.json`，并按下方配置说明调整。

配置文件不存在时，首次启动会自动生成默认配置。Docker Compose 启动前需先准备好配置文件。

### 3. 启动

**直接运行：**
```bash
./yggdrasil-proxy
```

**Docker：**
```bash
docker run -d -p 32217:32217 -v ./config.json:/app/config.json ghcr.io/deshark42/yggdrasil-proxy:latest
```

**Docker Compose（从 GHCR 拉取镜像）：**

仓库提供 `docker-compose.yml`。如果尚未创建配置文件，先复制示例，再启动：

```bash
cp config.example.json config.json
# 按需编辑 config.json；已有配置时跳过复制步骤
docker compose pull
docker compose up -d
```

配置文件以只读方式挂载；文件不存在时会报错，而不是自动创建同名目录。默认映射端口为 `32217:32217`，修改配置中的监听端口时应同步修改 Compose 的容器端口。

查看日志或停止服务：

```bash
docker compose logs -f
docker compose down
```

**可选：从本仓库源码构建**

准备好配置文件后，显式叠加 `docker-compose.build.yml`：

```bash
docker compose -f docker-compose.build.yml up -d --build
```

此方式从当前仓库构建，使用本地镜像名 `yggdrasil-proxy:local`，不会拉取或覆盖 GHCR 镜像标签；端口、配置挂载和重启策略沿用默认配置。后续重新构建、查看日志或停止服务时，可使用相同的两组 `-f` 参数。默认执行 `docker compose up -d` 不会加载这个构建选项。

### 4. 连接 Minecraft 服务器

在服务器 JVM 参数中添加：

```
-javaagent:authlib-injector-1.x.x.jar=http://你的代理地址:32217
```

## 配置说明

| 字段 | 说明 |
|------|------|
| `port` | 监听端口，默认 `32217` |
| `cache.ttlSeconds` | 关联自最近一次成功认证起的有效期（秒），默认 `86400`（24 小时） |
| `cache.cleanupIntervalSeconds` | 后台清理间隔（秒），默认 `600`（10 分钟） |
| `cache.maxEntries` | 最大关联条目数，默认 `10000` |
| `authServers` | 认证服务器列表，按 `priority` 从小到大依次尝试 |
| `priority` | 优先级，数值越小越优先 |
| `name` | 标识名，仅用于日志 |
| `url` | Yggdrasil 认证服务地址 |
| `timeout` | 请求超时时间（秒） |

缓存配置均为正整数；省略 `cache` 或其中的字段时使用相应默认值，因此旧配置无需修改。过期记录在下一次后台清理前可能仍占用空间，但查询时不会命中，且总条目数始终受容量上限约束。缓存不是整个容器的内存限额。

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
