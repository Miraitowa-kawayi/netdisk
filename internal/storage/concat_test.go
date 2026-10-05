package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// Concat 的用例：按给定顺序拼、边拼边算 hash、源缺失不留半成品、空列表可用。

func putRaw(t *testing.T, l *Local, key string, b []byte) {
	t.Helper()
	if _, err := l.Put(context.Background(), key, bytes.NewReader(b), int64(len(b))); err != nil {
		t.Fatalf("Put %s: %v", key, err)
	}
}

func readRaw(t *testing.T, l *Local, key string) []byte {
	t.Helper()
	rc, _, err := l.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("Open %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读 %s: %v", key, err)
	}
	return b
}

func TestConcatJoinsSourcesInGivenOrder(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()

	parts := [][]byte{[]byte("alpha-"), []byte("bravo-"), []byte("charlie")}
	keys := []string{"uploads/s/0", "uploads/s/1", "uploads/s/2"}
	for i, p := range parts {
		putRaw(t, l, keys[i], p)
	}

	want := bytes.Join(parts, nil)
	hash, size, err := l.Concat(ctx, "blobs/final", keys)
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	if size != int64(len(want)) {
		t.Errorf("size = %d, want %d", size, len(want))
	}
	if hash != wantHash(want) {
		t.Errorf("hash = %s, want %s", hash, wantHash(want))
	}
	if got := readRaw(t, l, "blobs/final"); !bytes.Equal(got, want) {
		t.Errorf("拼出来的内容 = %q, want %q", got, want)
	}
}

// 顺序由调用方给定，不做排序。
func TestConcatRespectsOrderNotContent(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()

	putRaw(t, l, "p/0", []byte("AAA"))
	putRaw(t, l, "p/1", []byte("BBB"))

	hash, _, err := l.Concat(ctx, "blobs/ba", []string{"p/1", "p/0"})
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	if hash != wantHash([]byte("BBBAAA")) {
		t.Errorf("按给定顺序应拼成 BBAAA，hash 不符")
	}
}

// 源缺失返回 ErrNotFound，且不留半成品。
func TestConcatMissingSourceLeavesNothing(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()

	putRaw(t, l, "p/0", []byte("present"))
	// p/1 不存在

	_, _, err := l.Concat(ctx, "blobs/final", []string{"p/0", "p/1"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("源缺失应 ErrNotFound，实得 %v", err)
	}
	if _, _, err := l.Open(ctx, "blobs/final"); !errors.Is(err, ErrNotFound) {
		t.Errorf("失败后不该留下目标对象，Open = %v", err)
	}
	// 盘上只应有那 1 个源对象，临时文件已清。
	if n := countFiles(t, l.dir); n != 1 {
		t.Errorf("盘上应只有 1 个源对象，实得 %d 个文件", n)
	}
}

func TestConcatEmptyListMakesEmptyObject(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()

	hash, size, err := l.Concat(ctx, "blobs/empty", nil)
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
	if hash != wantHash(nil) {
		t.Errorf("空内容的 hash 不符：%s", hash)
	}
}
