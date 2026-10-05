# 表结构设计说明

DDL 在 [`migrations/0001_init.sql`](../migrations/0001_init.sql)。这里记录为什么这么设计：任务书原话是“请在表结构的设计上多花些心思，不要为后续功能的实现埋雷”，所以每个决定都写清代价。

## 一、文件与文件夹用同一张表

`nodes` 表里用 `is_dir` 区分，`parent_id` 自引用成树。

分开两张表（`files` / `folders`）在写“列出目录内容”时就要 union 两次，“移动节点”要写两套，“分享一个节点”要写两套分支。合成一张表的代价是“目录不能有内容”这类约束要靠 CHECK 显式表达，也就是 DDL 里那条 `CHECK (NOT is_dir OR blob_id IS NULL)`。

## 二、元数据与内容分离：`nodes` vs `blobs`

`nodes` 是“用户看到的东西”（名字、位置、属于谁），`blobs` 是“磁盘上真实存在的一份内容”。

这一层分离换来三件事：

- 秒传：`blobs.content_hash` 上的唯一约束就是落点。客户端声明 hash，命中就只插一条 `nodes` 记录，字节一个都不传。
- 去重：多用户上传同一文件，磁盘上只有一份。
- 对象存储：迁移只是改 `blobs.backend` / `storage_key`，用户看到的目录结构完全不动。

## 三、不设 `ref_count` 计数列

判断“这份内容还有没有人用”有两种做法：

| 做法 | 代价 |
| --- | --- |
| 维护 `blobs.ref_count` 计数列 | 快；但任何一条漏掉的加减都会永久漂移，计数大于实际会导致 blob 永远删不掉，小于实际会误删还在用的文件 |
| 删除时 `NOT EXISTS (SELECT 1 FROM nodes WHERE blob_id = ?)` | 删除时多一次索引查找（`nodes_blob_idx` 就是为它建的）；但不存在漂移 |

选后者。这道题的规模下一次索引查找的成本可以忽略，而“计数漂移导致文件永远删不掉”是一个排查起来很痛苦的坑。如果以后真有 GC 压不动的那天，再在上层加计数缓存，而不是让它成为事实来源。

### 收尾时那个外键（0002 号迁移）

“现算”这条路的最后一步是 `DELETE FROM blobs`，这时会撞上 `nodes.blob_id → blobs.id` 的外键：已弱删的 `nodes` 行仍然握着 `blob_id`，外键（默认 NO ACTION）会直接挡下删除。所以 0002 号迁移把它改成 `ON DELETE SET NULL`，blob 行消失时，指向它的指针自动变 NULL。

安全性由回收 SQL 自己保证，不是靠外键：只有 `NOT EXISTS (SELECT 1 FROM nodes WHERE blob_id = ? AND deleted_at IS NULL)` 的行才会被删，存活引用一行都不会被误伤。外键在这里只负责“消灭悬空指针”，不负责判断该不该删。这也顺带说明了为什么回收要限定在这次被删的子树里，而不是“扫全库删所有没有被引用的 blob”，因为后者会误删一个刚 `UpsertBlob` 完、节点还没来得及插进去的并发上传（那是一段没有引用的合法窗口）。

## 四、根目录的重名：唯一索引里的 NULL 陷阱

同层不能重名，直觉写法是：

```sql
CREATE UNIQUE INDEX ON nodes (owner_id, parent_id, name);
```

这是错的：PostgreSQL 的唯一索引里 NULL 互不相等，所以根目录（`parent_id IS NULL`）下的重名会全部漏过。正确写法是用表达式索引把 NULL 换成一个哨兵值：

```sql
CREATE UNIQUE INDEX nodes_sibling_name_uniq
    ON nodes (owner_id, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
    WHERE deleted_at IS NULL;
```

### 别的数据库在这件事上怎么表现

- MySQL：和 PostgreSQL 一样允许多个 NULL（NULL 表示“未知”，未知 ≠ 未知），所以这个 `COALESCE` 写法换到 MySQL 一样需要。MySQL 在这件事上真正的坑在别处：默认排序规则（`utf8mb4_0900_ai_ci`）大小写不敏感，`a.txt` 和 `A.txt` 会撞唯一键；PostgreSQL 默认区分大小写。
- SQL Server：唯一索引里只允许一个 NULL，是把 NULL 当作相等来比较的，在那边反而不需要 `COALESCE`。
- PostgreSQL 15+：可以用 `UNIQUE NULLS NOT DISTINCT (owner_id, parent_id, name)` 让约束按“NULL 彼此相等”来判，语义比表达式索引更直白；本项目的写法两者都可以，选 `COALESCE` 是因为它同时兼容更老的 PG 版本。

（这三种行为都能用 psql 五分钟验完，见 `../02_知识点与自测.md` 的实验 2。）

## 五、弱删除，以及它对唯一索引的影响

`nodes.deleted_at` 为 NULL 才算“存在”。唯一索引带上 `WHERE deleted_at IS NULL`，于是删掉 `a.txt` 之后可以立刻重新建一个同名的 `a.txt`。

弱删的代价是每条查询都要记得带 `deleted_at IS NULL`（repository 层统一处理）。收获是“误删找回”变成一行 UPDATE，也方便以后做回收站。

## 六、分享与被分享对象被删除

`shares.node_id` 是 `ON DELETE CASCADE`，但那只对硬删生效，而 `nodes` 是弱删的，所以这一条并不能兜住主流程。约定是：

- 弱删 / 移动节点后：分享链接仍在，但访问时沿祖先链校验目标节点未删除（目录被弱删后，里面的文件虽然自己没被删，也不该还能通过旧链接下载）
- 硬删（purge）时：CASCADE 兜底清掉分享记录
- 分享的 `expires_at` 为 NULL 表示永不过期；`visit_count` 只做展示用

把这个行为在业务层明确写出来，而不是依赖数据库的级联，是因为“文件没了但分享链接还能下”是个安全问题。

## 七、分片上传的状态用两张表表达

- `upload_sessions`：一次“要上传某个文件”的意图（目标目录、文件名、总大小、分片大小、状态）
- `upload_parts`：已收到的分片，主键 `(session_id, part_no)`

主键的选择是有意的：重复上传同一个分片会变成幂等 upsert，断线重连时客户端不用关心“这一片到底传没传成功”。`upload_sessions.status` 用完即弃（`expires_at` 一天），过期会话由清理任务删除，分片文件随之回收。

## 八、索引与它们各自服务的查询

| 索引 | 服务的查询 |
| --- | --- |
| `nodes_owner_parent_idx` | 列目录：`WHERE owner_id = ? AND parent_id = ?` |
| `nodes_blob_idx` | 孤儿判定：`NOT EXISTS (... WHERE blob_id = ?)` |
| `nodes_sibling_name_uniq` | 同层重名检查（同时也是唯一性约束） |
| `blobs.content_hash`（UNIQUE） | 秒传命中 |
| `shares.token`（UNIQUE） | 按链接访问 |
| `upload_sessions_owner_idx` | “我有没有未完成的上传” |
| `upload_sessions_expires_idx` | 过期会话清理 |
