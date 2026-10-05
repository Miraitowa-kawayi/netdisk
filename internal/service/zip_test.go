package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/Miraitowa-kawayi/netdisk/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// P8 文件夹打包下载的业务契约。打真库（先 `make up`），库不可达时 Skip —— SKIP ≠ 过。
//
// 交付时这一组应该是**红的**：OpenZip 的遍历（Files.writeZip）还没实现，
// 红要红在"zip 里没东西 / 流报错"，而不是编译不过。
//
//	go test ./internal/service/ -run Zip -v
// ---------------------------------------------------------------------------

type zipFixture struct {
	files   *Files
	store   *repository.Store
	cleanDB *pgxpool.Pool
	ctx     context.Context
	owner   uuid.UUID
}

func newZipFixture(t *testing.T) *zipFixture {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = fallbackTestDSN
	}
	ctx := context.Background()

	st, err := repository.New(ctx, dsn)
	if err != nil {
		t.Skipf("跳过：建连接池失败（%v）", err)
	}
	if err := st.Ping(ctx); err != nil {
		st.Close()
		t.Skipf("跳过：数据库不可达（%v）—— 先跑 `make up`", err)
	}
	t.Cleanup(st.Close)

	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("跳过：清理用连接池建不起来（%v）", err)
	}
	t.Cleanup(db.Close)

	loc, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("建本地存储: %v", err)
	}
	f := &zipFixture{
		files:   NewFiles(st, loc, "local", slog.New(slog.NewTextHandler(io.Discard, nil))),
		store:   st,
		cleanDB: db,
		ctx:     ctx,
	}
	f.owner = f.newUser(t)
	return f
}

func (f *zipFixture) newUser(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := f.store.CreateUser(f.ctx, "test-"+uuid.NewString(), "not-a-real-bcrypt-hash", "")
	if err != nil {
		t.Fatalf("建测试用户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.cleanDB.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u.ID
}

func (f *zipFixture) upload(t *testing.T, parent *uuid.UUID, name, content string) model.Node {
	t.Helper()
	node, err := f.files.Upload(f.ctx, UploadInput{
		OwnerID:  f.owner,
		ParentID: parent,
		Name:     name,
		SizeHint: int64(len(content)),
		Body:     strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("上传 %s: %v", name, err)
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	t.Cleanup(func() {
		_, _ = f.cleanDB.Exec(context.Background(), `DELETE FROM blobs WHERE content_hash = $1`, hash)
	})
	return node
}

func (f *zipFixture) mkdir(t *testing.T, parent *uuid.UUID, name string) model.Node {
	t.Helper()
	dir, err := f.files.CreateDir(f.ctx, f.owner, parent, name)
	if err != nil {
		t.Fatalf("建目录 %s: %v", name, err)
	}
	return dir
}

// openZip 调 OpenZip 并把整条 zip 流读回来解成 名字→内容 的表。
// 目录条目以 '/' 结尾，其内容为空串。
func (f *zipFixture) openZip(t *testing.T, id uuid.UUID) (string, map[string]string) {
	t.Helper()
	z, err := f.files.OpenZip(f.ctx, f.owner, id)
	if err != nil {
		t.Fatalf("OpenZip: %v", err)
	}
	defer z.Content.Close()

	raw, err := io.ReadAll(z.Content)
	if err != nil {
		t.Fatalf("读 zip 流: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("zip 流不是合法压缩包（%d 字节）: %v", len(raw), err)
	}

	entries := make(map[string]string, len(zr.File))
	for _, file := range zr.File {
		if file.FileInfo().IsDir() {
			entries[file.Name] = ""
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("打开条目 %q: %v", file.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("读条目 %q: %v", file.Name, err)
		}
		entries[file.Name] = string(body)
	}
	return z.Name, entries
}

func TestZipFolderTree(t *testing.T) {
	f := newZipFixture(t)
	root := f.mkdir(t, nil, "docs")
	f.upload(t, &root.ID, "a.txt", "AAA")
	sub := f.mkdir(t, &root.ID, "sub")
	f.upload(t, &sub.ID, "b.txt", "BBB")
	f.mkdir(t, &root.ID, "empty")
	f.upload(t, &root.ID, "中文.txt", "chinese")

	name, entries := f.openZip(t, root.ID)
	if name != "docs.zip" {
		t.Errorf("建议文件名 = %q, want docs.zip", name)
	}

	want := map[string]string{
		"docs/a.txt":     "AAA",
		"docs/sub/b.txt": "BBB",
		"docs/中文.txt":    "chinese",
	}
	for entry, body := range want {
		got, ok := entries[entry]
		if !ok {
			t.Errorf("zip 里缺条目 %q（实得 %v）", entry, sortedKeys(entries))
			continue
		}
		if got != body {
			t.Errorf("条目 %q 内容 = %q, want %q", entry, got, body)
		}
	}

	// 目录条目：sub 与 empty（空的也得有）都要在，且名字以 '/' 结尾。
	for _, dir := range []string{"docs/sub/", "docs/empty/"} {
		body, ok := entries[dir]
		if !ok {
			t.Errorf("缺目录条目 %q（实得 %v）", dir, sortedKeys(entries))
			continue
		}
		if body != "" {
			t.Errorf("目录条目 %q 不该有内容，实得 %q", dir, body)
		}
	}

	// 顶层前缀必须是 root.Name + "/"，且整棵树不许出现前导 '/' 的条目名。
	for entry := range entries {
		if strings.HasPrefix(entry, "/") {
			t.Errorf("条目名不能以 '/' 开头（Windows 资源管理器会打不开）：%q", entry)
		}
		if !strings.HasPrefix(entry, "docs/") {
			t.Errorf("条目 %q 不在 docs/ 前缀下", entry)
		}
	}
}

func TestZipEmptyFolder(t *testing.T) {
	f := newZipFixture(t)
	root := f.mkdir(t, nil, "blank")

	_, entries := f.openZip(t, root.ID)
	if len(entries) != 1 {
		t.Fatalf("空目录的 zip 应该只有一条目录项，实得 %v", sortedKeys(entries))
	}
	if _, ok := entries["blank/"]; !ok {
		t.Errorf("空目录缺 %q 这条（实得 %v）", "blank/", sortedKeys(entries))
	}
}

func TestZipRejectsFile(t *testing.T) {
	f := newZipFixture(t)
	file := f.upload(t, nil, "solo.txt", "X")

	if _, err := f.files.OpenZip(f.ctx, f.owner, file.ID); !errors.Is(err, ErrNotADirectory) {
		t.Fatalf("对文件调 OpenZip：err = %v, want ErrNotADirectory", err)
	}
}

func TestZipMissingIsNotFound(t *testing.T) {
	f := newZipFixture(t)

	if _, err := f.files.OpenZip(f.ctx, f.owner, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的 id：err = %v, want ErrNotFound", err)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
