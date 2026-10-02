# Dev Log

## 2026-10-01 · D0：项目骨架

**做了什么**

- 定技术栈：Go 1.27 + chi（路由）+ pgx（直接手写 SQL，不上 ORM）+ PostgreSQL 16（docker compose）
- 搭出分层骨架：`cmd/server` 入口 → `internal/{config,handler,service,repository,storage,model,middleware,httpx}`
- 表设计定稿，落成 `migrations/0001_init.sql`（6 张表 + 索引），设计理由写在 `docs/schema.md`
- 统一错误响应格式 `{"error":{"code","message"}}`；`/healthz`（不碰依赖）与 `/readyz`（要求数据库可用）分开
- 端到端跑通：`docker compose up -d postgres` → 服务起来 → 两个健康检查都是 200

**卡在哪 / 踩到的坑**

1. **8080 端口被占用**：本机已经跑着一个 adminer 容器占着 8080，服务直接
   `listen tcp :8080: bind: address already in use` 退出了。改用 8081。
   教训是"能绑定成功"本身要当成一条验证项，不能默认端口是空的。由这个问题，我在思考如何
   集成已经使用过的端口，或者说已经有这类项目我能直接调用的
2. **唯一索引里的 NULL 陷阱**：想让"同一目录下不能有同名文件"，
   直觉写法是给 `nodes (owner_id, parent_id, name)` 建唯一索引。但 PostgreSQL 的唯一索引里
   NULL 互不相等，而根目录的 `parent_id` 就是 NULL —— 于是**根目录下的重名全部漏过**。
   最后改成 `COALESCE(parent_id, '<nil-uuid>')` 的表达式索引
   （用 psql 建三张表实测过：直觉写法能插进两条重名，COALESCE 和 PG 15+ 的
   `UNIQUE NULLS NOT DISTINCT` 都会拒绝）。
   顺带弄清楚了一件我一开始就有点混乱的事：**这一点 MySQL 和 PostgreSQL 是一样的**（唯一索引里都
   允许多个 NULL，因为 NULL 表示"未知"，未知 ≠ 未知）。真正不一样的是 SQL Server（只允许
   一个 NULL）。MySQL 在这件事上真正的bug是**默认排序规则大小写不敏感**
   （utf8mb4_0900_ai_ci），`a.txt` 和 `A.txt` 会撞唯一键；但 PG 默认区分大小写。
3. **秒传的引用计数要不要存**：一开始想给 `blobs` 加 `ref_count` 列，后来改成删除时用
   `NOT EXISTS (SELECT 1 FROM nodes WHERE blob_id = ?)` 现算。理由是计数列一旦漏了一次加减
   就会永久漂移，最后表现为"文件明明删光了，磁盘上的内容却永远删不掉"。
   多一次索引查找换掉一整类排查起来很痛的 bug。

**当时记的下一步**

- P1：文件的上传/下载/重命名/删除/列表，全程流式（不能把请求体读进内存）
- P2：注册登录 + JWT 中间件，把 P1 的接口保护起来

## 2026-10-02 · D1：P1 文件接口 + P2 鉴权

**做了什么**

- P2 全量：`POST /api/v1/auth/register`、`/login`、`GET /auth/me`；HS256 JWT 中间件
  把 `/api/v1/files` 整套保护起来。密码走 bcrypt；登录失败不区分"用户不存在/密码错"，
  且两条路径都过一次 bcrypt，避免用响应时间枚举用户名。
- P1：上传（multipart **流式**，边收边写）、下载（`http.ServeContent`，Range/206 白送）、
  列表（每个文件带 `download_url`）、改名、删除（弱删，只打 `deleted_at`）。
- 表结构没动：D0 定的 `nodes`（树）+ `blobs`（内容）正好接得上 —— 内容按 SHA-256 去重，
  同一份内容磁盘上只留一个对象。

**分工**：P2 和 P1 的 SQL / 路由 / DTO / 错误映射由 Agent 搭；
**`internal/storage/local.go` 的 `Put` / `Open`（这一天的流式核心）**，
验收契约是 `internal/storage/local_test.go`。

**curl 验证过的行为**

| 场景 | 结果 |
| --- | --- |
| 无 token 访问 `/api/v1/files` | 401 |
| 注册 / 重复注册 | 201 / 409 |
| 密码错、用户不存在 | 都是同一句 401 |
| 登录 → `/me`；伪造 token | 200；401 |
| 空根目录列表 | 200 `{"files":[]}` |
| `parent_id` 不是 UUID | 400 |
| 改名/删除不存在的节点 | 404 |
| 上传 | 500（`Put` 还是占位实现） |

**下一步**

- 实现 `local.Put` / `local.Open` → `go test ./internal/storage/ -v` 全绿
- 然后跑 D1 的判据：传 500MB，`docker stats` 里内存不随文件大小涨

## 2026-10-03 · D1 收尾：流式核心 + 500MB 验证

**做了什么**

- 实现 `internal/storage/local.go` 的 `Put` / `Open`（D1 的流式核心是我自己写的、Agent 逐行 review，提出修改意见我修改完成的）：
  - `Put`：`io.MultiWriter(tempFile, hasher)` 边读边写盘、顺路算 SHA-256，全程没有 `io.ReadAll`；
    临时文件用 `os.CreateTemp(目标目录, ".put-*")` 建在**目标文件所在目录**（同一文件系统，
    `os.Rename` 才是原子的）→ `Sync` → `Close` → `Rename`；中途出错用 defer 清掉临时文件，
    不留半成品。
  - `Open`：`os.Open` + `Stat` 组装 `ObjectInfo`；`*os.File` 天生可 Seek，`http.ServeContent`
    的 Range/206 靠它；对象不存在时翻成 `ErrNotFound`。
- `go test ./internal/storage/ -v` **7/7 通过**，`go build` / `go vet` / `gofmt` 都干净。

**500MB 流式验证**

| | |
| --- | --- |
| 文件 | `/tmp/big500.bin`，500,000,000 字节（477M） |
| server 进程 RSS | 空闲 14,363 KB → 上传中峰值 14,572 KB（**+209 KB**） |

差约 2400 倍 —— 内存不随对象大小涨，D1 的判据满足。落盘的 blob
（`data/blobs/blobs/ed0e4078-…`）也确实是 477M，不是传一半假装成功。

⚠️ 计划里"用 `docker stats` 看内存"这句要改：进容器的只有 postgres，**Go 服务跑在宿主机**，
`docker stats` 里根本没有它 —— 要看的是宿主机上 server 进程的 RSS（`ps -o rss= -p <真身 pid>`，
`go run` 的父进程不是真身）。

**卡在哪 / 踩到的坑**

1. 第一版 `Open` 编不过：`f` 和 `info` 都组装好了，**却忘了替换末尾那句占位 `return`**，
   连带 `info` 声明未用、还用了 `ObjectInfo` 里不存在的 `ModTime` 字段。根因是
   **忘记 `storage.go` 的接口定义就动手** —— 教训是写实现前先把契约文件读一遍。
2. 重传同名文件被 409 挡住（`another entry with the same name already exists here`）：
   是 D0 那条 `COALESCE(parent_id, '<nil-uuid>')` 唯一索引在起作用，不是 bug；顺便确认了
   弱删（只打 `deleted_at`）能让名字重新可用。

**下一步**

- D2（P3 文件夹树）：新建 / 移动 / 列子项，移动时的**环检测**、
  删除文件夹的级联语义。判据：把文件夹移进自己的子目录被**明确拒绝**。
