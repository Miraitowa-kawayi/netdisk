-- NetDisk 初始表结构
--
-- 设计要点：
--   1. 文件与文件夹**同一张表** nodes，用 parent_id 自引用成树。分开两张表会让
--      "移动"、"列目录"、"分享"这些操作全部要写两遍。
--   2. 内容与元数据分离：nodes 是"用户看到的东西"，blobs 是"磁盘上真实存在的一份内容"。
--      blobs.content_hash 上的唯一约束就是秒传的落点。
--   3. **不设 ref_count 计数列**。引用关系的事实来源是 nodes.blob_id 本身，
--      删到最后一个引用时用 `NOT EXISTS (SELECT 1 FROM nodes WHERE blob_id = ...)` 判定。
--      代价是删除多一次索引查找；收益是不存在"计数漂移导致 blob 永远删不掉"的bug。
--   4. 根目录用 parent_id IS NULL 表示 —— 但 NULL 在唯一索引里互不相等，
--      所以同名唯一约束必须用 COALESCE 表达式索引（见下方 nodes_sibling_name_uniq）。
--   5. 弱删除：nodes.deleted_at 为 NULL 才算"存在"。唯一索引也带这个条件，
--      于是删除后可以重新创建同名文件。
--   6. shares → nodes 是 ON DELETE CASCADE，只对**硬删**生效；弱删时的分享失效
--      由业务层在读取时校验（见 docs/schema.md）。

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

-- ---------------------------------------------------------------------------
-- 用户
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,          -- bcrypt，绝不存明文
    nickname      TEXT        NOT NULL DEFAULT '',
    bio           TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- 内容块：磁盘上真实存在的一份文件内容（按 SHA-256 去重）
-- ---------------------------------------------------------------------------
CREATE TABLE blobs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_hash TEXT        NOT NULL UNIQUE,    -- sha256 hex，秒传/去重的键
    size         BIGINT      NOT NULL,
    backend      TEXT        NOT NULL DEFAULT 'local',  -- local | s3（P6）
    storage_key  TEXT        NOT NULL,           -- 后端内部的定位键
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- 节点：文件与文件夹统一成树
-- ---------------------------------------------------------------------------
CREATE TABLE nodes (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id   UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id  UUID        REFERENCES nodes (id) ON DELETE CASCADE,  -- NULL = 根目录
    name       TEXT        NOT NULL,
    is_dir     BOOLEAN     NOT NULL,
    size       BIGINT      NOT NULL DEFAULT 0,   -- 目录恒为 0
    blob_id    UUID        REFERENCES blobs (id),  -- 文件指向内容；目录为 NULL
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,                      -- 弱删

    -- 目录不能挂内容；文件在"上传完成"时才建节点，所以不限制 blob_id 非空
    CONSTRAINT nodes_dir_has_no_blob CHECK (NOT is_dir OR blob_id IS NULL)
);

-- 同层同名唯一。COALESCE 是必需的：唯一索引里 NULL 互不相等，
-- 直接写 (owner_id, parent_id, name) 会让根目录下的重名全部漏过。
CREATE UNIQUE INDEX nodes_sibling_name_uniq
    ON nodes (owner_id, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
    WHERE deleted_at IS NULL;

-- 列目录：WHERE owner_id = ? AND parent_id = ?
CREATE INDEX nodes_owner_parent_idx ON nodes (owner_id, parent_id) WHERE deleted_at IS NULL;

-- 孤儿判定：NOT EXISTS (SELECT 1 FROM nodes WHERE blob_id = ?)
CREATE INDEX nodes_blob_idx ON nodes (blob_id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- 分享链接
-- ---------------------------------------------------------------------------
CREATE TABLE shares (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token       TEXT        NOT NULL UNIQUE,     -- 公开访问用的随机串
    node_id     UUID        NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    created_by  UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ,                     -- NULL = 永不过期
    visit_count BIGINT      NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX shares_node_idx ON shares (node_id);

-- ---------------------------------------------------------------------------
-- 分片上传（P7 断点续传的状态）
--   会话 = 一次"要上传某个文件"的意图；分片表记录已收到的片。
--   PRIMARY KEY (session_id, part_no) 让重复上传同一分片变成幂等 upsert。
-- ---------------------------------------------------------------------------
CREATE TABLE upload_sessions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id    UUID        REFERENCES nodes (id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    total_size   BIGINT      NOT NULL,
    chunk_size   BIGINT      NOT NULL,
    part_count   INTEGER     NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'pending',  -- pending | completed | aborted
    content_hash TEXT,                                    -- 客户端预申报时可直接秒传
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL DEFAULT now() + interval '1 day'
);

CREATE INDEX upload_sessions_owner_idx ON upload_sessions (owner_id) WHERE status = 'pending';

CREATE TABLE upload_parts (
    session_id UUID        NOT NULL REFERENCES upload_sessions (id) ON DELETE CASCADE,
    part_no    INTEGER     NOT NULL,     -- 从 0 开始
    size       BIGINT      NOT NULL,
    checksum   TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, part_no)
);

-- 会话过期清理用：DELETE FROM upload_sessions WHERE expires_at < now()
CREATE INDEX upload_sessions_expires_idx ON upload_sessions (expires_at);
