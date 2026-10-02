# NetDisk

一个用 Go 写的网盘服务端：文件的上传下载、文件夹树、分享链接、多用户与秒传去重、断点续传。

> 项目为冰岩作坊实习任务书而做，开发过程记录在 [devlog.md](./devlog.md)。

## 当前进度

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| P0 | 项目骨架、表结构、统一错误格式、健康检查 | ✅ |
| P1 | 文件上传/下载/重命名/删除/列表（流式 IO） | ✅ |
| P2 | 用户注册登录、JWT 鉴权 | ✅ |
| P3 | 文件夹树：新建/更名/删除/移动 | ⬜ |
| P4 | 分享链接 | ⬜ |
| P5 | 多用户隔离 + 秒传去重 | ⬜ |
| P6 | 对象存储后端（S3 兼容） | ⬜ |
| P7 | 断点续传：Range 下载 + 分片上传 | ⬜ |
| P8 | 文件夹打包下载 | ⬜ |
| P9 | NFS 映射 | ⬜ |
| P10 | P2P 直传 | ⬜ |

## 技术栈

- **Go 1.27** / 标准库 `net/http`
- **chi**：路由（贴近标准库，中间件生态小而成体系）
- **pgx/v5**：直接手写 SQL，不引 ORM 
- **PostgreSQL 16**：`docker compose` 起
- **JWT**（`golang-jwt/jwt/v5`）：鉴权

## 快速开始

```bash
# 1. 起数据库（migrations/ 会在数据卷为空时自动执行）
docker compose up -d postgres

# 2. 配置
cp .env.example .env      # 首次

# 3. 跑起来
go run ./cmd/server
```

验证：

```bash
curl -i localhost:8081/healthz   # 200 {"status":"ok"}，不碰数据库
curl -i localhost:8081/readyz    # 200 {"status":"ready"}；数据库不可用时 503
```

也可以直接用 Makefile：`make up && make run`。

### 重置数据库

`migrations/` 里的脚本只在**数据卷为空时**执行一次。改了 DDL 要重来：

```bash
docker compose down -v && docker compose up -d postgres
```

## 目录结构

```
cmd/server/          程序入口：装配依赖、起 HTTP 服务、优雅退出
internal/
  config/            配置（.env + 环境变量）
  handler/           HTTP 层：路由、参数校验、状态码
  service/           业务逻辑层：规则与事务边界
  repository/        数据访问层：所有 SQL
  storage/           内容存取接口（local / s3 两种实现）
  model/             表结构在 Go 侧的映射
  middleware/        请求日志、panic 兜底
  httpx/             统一响应格式与错误码
migrations/          数据库 DDL
docs/                设计说明与 API 文档
```

分层的依赖方向是单向的：`handler → service → repository / storage`。
上层只依赖下层的接口，所以 P6 加对象存储时不需要回头改 P1 的代码。

## 约定

- **错误响应**统一为 `{"error":{"code":"...","message":"..."}}`，`code` 对外稳定
- **健康检查**分两个：`/healthz` 只表示进程活着（探活用），`/readyz` 要求依赖可用（不要拿它做保活探针）
- **提交信息**遵循 [Conventional Commits](https://www.conventionalcommits.org/)，小步提交
- 不用 `io.ReadAll` 处理请求体或文件内容 —— 见 [docs/schema.md](./docs/schema.md) 同类说明

## 后续计划

- 用 `golang-migrate` 替换"初始脚本"式的建表方式（表结构开始频繁变动时）
- 对象存储后端、NFS 映射、P2P 直传（P9/P10）
