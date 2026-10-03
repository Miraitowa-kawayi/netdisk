-- 0002: nodes.blob_id 的外键改成 ON DELETE SET NULL
--
-- 起因（D3 秒传 / 引用计数）：内容回收要 DELETE blobs 行，而"只被已弱删节点引用"
-- 的 blob 正是合法回收对象。原外键是 NO ACTION，已弱删的 nodes 行仍然握着 blob_id，
-- 于是 DELETE 被外键挡下（实测 23503）：
--
--   ERROR: update or delete on table "blobs" violates foreign key constraint
--          "nodes_blob_id_fkey" on table "nodes"
--   DETAIL: Key (id)=(...) is still referenced from table "nodes".
--
-- 改成 ON DELETE SET NULL：blob 行消失时，指向它的指针自动变 NULL。
-- 存活节点不会被误伤 —— 回收 SQL 里用 NOT EXISTS(... deleted_at IS NULL)
-- 保证"还有活引用就一行都不删"（见 repository.ReclaimOrphanBlobs）。
--
-- 幂等：先 DROP IF EXISTS 再 ADD，重复执行不会报错。
ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_blob_id_fkey;

ALTER TABLE nodes ADD CONSTRAINT nodes_blob_id_fkey
    FOREIGN KEY (blob_id) REFERENCES blobs (id) ON DELETE SET NULL;
