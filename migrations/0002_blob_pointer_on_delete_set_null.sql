-- 0002: nodes.blob_id 外键改为 ON DELETE SET NULL。
-- blobs 行被删除时指向它的指针自动置空，存活节点不受影响
-- （回收 SQL 用 NOT EXISTS(... deleted_at IS NULL) 保证还有活引用就不删）。
-- 幂等：先 DROP IF EXISTS 再 ADD。
ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_blob_id_fkey;

ALTER TABLE nodes ADD CONSTRAINT nodes_blob_id_fkey
    FOREIGN KEY (blob_id) REFERENCES blobs (id) ON DELETE SET NULL;
