-- NetDisk 初始表结构

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

-- 用户
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,          -- bcrypt
    nickname      TEXT        NOT NULL DEFAULT '',
    bio           TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 内容块：按 SHA-256 去重
CREATE TABLE blobs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_hash TEXT        NOT NULL UNIQUE,    -- sha256 hex，去重键
    size         BIGINT      NOT NULL,
    backend      TEXT        NOT NULL DEFAULT 'local',  -- local | s3
    storage_key  TEXT        NOT NULL,           -- 后端内部定位键
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 节点：文件与文件夹统一成树
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
    deleted_at TIMESTAMPTZ,                      -- 软删

    -- 目录不能挂内容
    CONSTRAINT nodes_dir_has_no_blob CHECK (NOT is_dir OR blob_id IS NULL)
);

-- 同层同名唯一；COALESCE 必需（唯一索引中 NULL 互不相等，否则根目录重名会被漏过）。
CREATE UNIQUE INDEX nodes_sibling_name_uniq
    ON nodes (owner_id, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
    WHERE deleted_at IS NULL;

-- 列目录：WHERE owner_id = ? AND parent_id = ?
CREATE INDEX nodes_owner_parent_idx ON nodes (owner_id, parent_id) WHERE deleted_at IS NULL;

-- 孤儿判定：NOT EXISTS (SELECT 1 FROM nodes WHERE blob_id = ?)
CREATE INDEX nodes_blob_idx ON nodes (blob_id) WHERE deleted_at IS NULL;

-- 分享链接
CREATE TABLE shares (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token       TEXT        NOT NULL UNIQUE,     -- 公开访问凭据
    node_id     UUID        NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    created_by  UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ,                     -- NULL = 永不过期
    visit_count BIGINT      NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX shares_node_idx ON shares (node_id);

-- 分片上传：会话记录一次上传意图，分片表记录已收到的片（主键使其幂等）
CREATE TABLE upload_sessions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id    UUID        REFERENCES nodes (id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    total_size   BIGINT      NOT NULL,
    chunk_size   BIGINT      NOT NULL,
    part_count   INTEGER     NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'pending',  -- pending | completed | aborted
    content_hash TEXT,                                    -- 客户端预申报的整份 hash
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
