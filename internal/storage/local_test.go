package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var putFixture = bytes.Repeat([]byte("netdisk-streaming-"), 4096) // ~72KB

func wantHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newTestLocal(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return l
}

// countFiles 数根目录下的文件数；临时文件未清理会在此暴露。
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return n
}

func TestPutStoresWholeObjectAndReturnsHash(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()

	got, err := l.Put(ctx, "blobs/a", bytes.NewReader(putFixture), int64(len(putFixture)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := wantHash(putFixture); got != want {
		t.Errorf("hash = %q, want %q", got, want)
	}

	info, err := l.Stat(ctx, "blobs/a")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(putFixture)) {
		t.Errorf("size = %d, want %d", info.Size, len(putFixture))
	}
	if n := countFiles(t, l.dir); n != 1 {
		t.Errorf("根目录下文件数 = %d, want 1（有临时文件没清掉？）", n)
	}
}

func TestPutAcceptsUnknownSize(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()

	if _, err := l.Put(ctx, "blobs/size-unknown", bytes.NewReader(putFixture), -1); err != nil {
		t.Fatalf("Put(size=-1): %v", err)
	}
	info, err := l.Stat(ctx, "blobs/size-unknown")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(putFixture)) {
		t.Errorf("size = %d, want %d", info.Size, len(putFixture))
	}
}

// errReader 在读时抛错，模拟传输中断。
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestPutOnMidwayFailureLeavesNothing(t *testing.T) {
	l := newTestLocal(t)
	boom := errors.New("boom")
	body := io.MultiReader(bytes.NewReader(putFixture[:1024]), errReader{boom})

	if _, err := l.Put(context.Background(), "blobs/b", body, -1); !errors.Is(err, boom) {
		t.Fatalf("Put err = %v, want %v", err, boom)
	}

	if n := countFiles(t, l.dir); n != 0 {
		t.Errorf("失败后根目录下还剩 %d 个文件, want 0（半成品或临时文件泄漏）", n)
	}
}

func TestOpenReturnsSeekableReader(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()

	if _, err := l.Put(ctx, "blobs/c", bytes.NewReader(putFixture), -1); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, info, err := l.Open(ctx, "blobs/c")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	if info.Size != int64(len(putFixture)) {
		t.Errorf("info.Size = %d, want %d", info.Size, len(putFixture))
	}

	const offset = 100
	if _, err := rc.Seek(offset, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(buf, putFixture[offset:offset+16]) {
		t.Errorf("Seek 后读到 %q, want %q", buf, putFixture[offset:offset+16])
	}
}

func TestOpenMissingReturnsErrNotFound(t *testing.T) {
	l := newTestLocal(t)
	if _, _, err := l.Open(context.Background(), "blobs/nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open(不存在的 key) err = %v, want ErrNotFound", err)
	}
}

func TestDeleteThenStatNotFound(t *testing.T) {
	l := newTestLocal(t)
	ctx := context.Background()

	// 直接落文件造对象，不经过 Put。
	p, err := l.keyPath("blobs/d")
	if err != nil {
		t.Fatalf("keyPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, putFixture, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := l.Delete(ctx, "blobs/d"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := l.Stat(ctx, "blobs/d"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删后再 Stat = %v, want ErrNotFound", err)
	}
	if err := l.Delete(ctx, "blobs/d"); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复 Delete = %v, want ErrNotFound", err)
	}
}

func TestKeyCannotEscapeRoot(t *testing.T) {
	l := newTestLocal(t)
	p, err := l.keyPath("../../etc/passwd")
	if err != nil {
		return // 直接报错也算安全
	}
	rel, err := filepath.Rel(l.dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Errorf("keyPath(%q) = %q，逃出了根目录", "../../etc/passwd", p)
	}
}
