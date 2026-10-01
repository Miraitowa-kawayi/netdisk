# Dev Log

> 记录开发过程。每节写三件事：**做了什么 / 卡在哪 / 下一步**。
>
> ⚠️ 这份日志是提交物的一部分，验收时会看。事实清单可以直接用，但**"卡在哪"和感受请用你自己的话写一遍** ——
> 验收（本质是面试）会追问"这块当时你怎么想的"，日志里写不出来的部分就是到时候讲不出来的部分。

---

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
   教训是"能绑定成功"本身要当成一条验证项，不能默认端口是空的。
2. **唯一索引里的 NULL 陷阱**（这次最值得记的一条）：想让"同一目录下不能有同名文件"，
   直觉写法是给 `nodes (owner_id, parent_id, name)` 建唯一索引。但 PostgreSQL 的唯一索引里
   NULL 互不相等，而根目录的 `parent_id` 就是 NULL —— 于是**根目录下的重名全部漏过**。
   最后改成 `COALESCE(parent_id, '<nil-uuid>')` 的表达式索引。
   顺带发现 MySQL 的行为不一样（NULL 也参与比较），同一个写法换数据库结论就变。
3. **秒传的引用计数要不要存**：一开始想给 `blobs` 加 `ref_count` 列，后来改成删除时用
   `NOT EXISTS (SELECT 1 FROM nodes WHERE blob_id = ?)` 现算。理由是计数列一旦漏了一次加减
   就会永久漂移，最后表现为"文件明明删光了，磁盘上的内容却永远删不掉"。
   多一次索引查找换掉一整类排查起来很痛的 bug。

**下一步（D1）**

- P1：文件的上传/下载/重命名/删除/列表，全程流式（不能把请求体读进内存）
- P2：注册登录 + JWT 中间件，把 P1 的接口保护起来
