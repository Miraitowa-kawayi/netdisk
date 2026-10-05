package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/Miraitowa-kawayi/netdisk/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 分享链接的测试打真库（先跑 `make up`），库不可达时跳过。

type shareFixture struct {
	shares  *Shares
	files   *Files
	store   *repository.Store
	cleanDB *pgxpool.Pool
	ctx     context.Context
	owner   uuid.UUID
}

func newShareFixture(t *testing.T) *shareFixture {
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
		t.Skipf("跳过：数据库不可达（%v）；先跑 `make up`", err)
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	files := NewFiles(st, loc, "local", logger)

	f := &shareFixture{
		shares:  NewShares(st, files, logger),
		files:   files,
		store:   st,
		cleanDB: db,
		ctx:     ctx,
	}
	f.owner = f.newUser(t)
	return f
}

func (f *shareFixture) newUser(t *testing.T) uuid.UUID {
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

// upload 建一个文件节点，并登记用例结束后删除它的 blob 行（blob 不随用户级联删除）。
func (f *shareFixture) upload(t *testing.T, parent *uuid.UUID, name, content string) model.Node {
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

func (f *shareFixture) mkdir(t *testing.T, parent *uuid.UUID, name string) model.Node {
	t.Helper()
	dir, err := f.files.CreateDir(f.ctx, f.owner, parent, name)
	if err != nil {
		t.Fatalf("建目录 %s: %v", name, err)
	}
	return dir
}

func (f *shareFixture) share(t *testing.T, owner, nodeID uuid.UUID) ShareView {
	t.Helper()
	view, err := f.shares.Create(f.ctx, ShareInput{OwnerID: owner, NodeID: nodeID})
	if err != nil {
		t.Fatalf("建分享: %v", err)
	}
	return view
}

// fetch 走匿名下载路径，把内容读出来。
func (f *shareFixture) fetch(t *testing.T, token string, nodeID *uuid.UUID) ([]byte, error) {
	t.Helper()
	dl, err := f.shares.OpenDownload(f.ctx, token, nodeID)
	if err != nil {
		return nil, err
	}
	defer dl.Content.Close()
	return io.ReadAll(dl.Content)
}

func TestShareFileRoundTrip(t *testing.T) {
	f := newShareFixture(t)
	const body = "hello share"
	file := f.upload(t, nil, "hello.txt", body)

	view := f.share(t, f.owner, file.ID)
	if view.URL != "/api/v1/share/"+view.Share.Token {
		t.Errorf("url = %q, 和 token 对不上", view.URL)
	}

	pv, err := f.shares.Resolve(f.ctx, view.Share.Token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pv.Node.ID != file.ID || pv.Node.Name != "hello.txt" || pv.Node.Size != int64(len(body)) {
		t.Errorf("Resolve 回来的节点不对: %+v", pv.Node)
	}
	if len(pv.Children) != 0 {
		t.Errorf("文件分享不该有 children，实得 %d 个", len(pv.Children))
	}

	got, err := f.fetch(t, view.Share.Token, nil)
	if err != nil {
		t.Fatalf("匿名下载: %v", err)
	}
	if string(got) != body {
		t.Errorf("下载内容 = %q, want %q", got, body)
	}

	// 访问一次之后计数应该 +1。
	row, err := f.store.GetShareByToken(f.ctx, view.Share.Token)
	if err != nil {
		t.Fatalf("取分享行: %v", err)
	}
	if row.VisitCount != 1 {
		t.Errorf("visit_count = %d, want 1", row.VisitCount)
	}
}

func TestShareFolderListsChildrenAndGatesDescendants(t *testing.T) {
	f := newShareFixture(t)
	dir := f.mkdir(t, nil, "public")
	inside := f.upload(t, &dir.ID, "a.txt", "AAA")
	sub := f.mkdir(t, &dir.ID, "sub")
	deep := f.upload(t, &sub.ID, "b.txt", "BBB")
	outside := f.upload(t, nil, "secret.txt", "SECRET")

	view := f.share(t, f.owner, dir.ID)

	pv, err := f.shares.Resolve(f.ctx, view.Share.Token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !pv.Node.IsDir {
		t.Fatal("分享的应该是个目录")
	}
	names := map[string]bool{}
	for _, c := range pv.Children {
		names[c.Name] = true
	}
	if !names["a.txt"] || !names["sub"] {
		t.Errorf("目录分享应列出直接子项 a.txt 与 sub，实得 %v", names)
	}
	if names["secret.txt"] {
		t.Error("别处的文件不该出现在这棵子树的列表里")
	}

	t.Run("直接子项可下", func(t *testing.T) {
		got, err := f.fetch(t, view.Share.Token, &inside.ID)
		if err != nil || string(got) != "AAA" {
			t.Errorf("下直接子项: %q err=%v", got, err)
		}
	})

	t.Run("孙节点也可下（在这棵子树里）", func(t *testing.T) {
		got, err := f.fetch(t, view.Share.Token, &deep.ID)
		if err != nil || string(got) != "BBB" {
			t.Errorf("下孙节点: %q err=%v", got, err)
		}
	})

	t.Run("子树外的节点一律 NotFound", func(t *testing.T) {
		if _, err := f.fetch(t, view.Share.Token, &outside.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("下子树外的文件应 ErrNotFound，实得 %v", err)
		}
		if _, err := f.fetch(t, view.Share.Token, &uuid.UUID{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("随便造个 id 也应 ErrNotFound，实得 %v", err)
		}
	})

	t.Run("目录本身不是文件", func(t *testing.T) {
		if _, err := f.fetch(t, view.Share.Token, nil); !errors.Is(err, ErrNotAFile) {
			t.Errorf("直接下目录应 ErrNotAFile，实得 %v", err)
		}
	})
}

func TestShareDiesWhenAncestorDeleted(t *testing.T) {
	f := newShareFixture(t)
	dir := f.mkdir(t, nil, "docs")
	file := f.upload(t, &dir.ID, "inside.txt", "INSIDE")

	view := f.share(t, f.owner, file.ID)
	if _, err := f.shares.Resolve(f.ctx, view.Share.Token); err != nil {
		t.Fatalf("删之前应该能访问: %v", err)
	}

	// 删的是父目录，文件本身没被删。
	if err := f.files.Delete(f.ctx, f.owner, dir.ID); err != nil {
		t.Fatalf("删父目录: %v", err)
	}

	if _, err := f.shares.Resolve(f.ctx, view.Share.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("祖先被删后 Resolve 应 ErrNotFound，实得 %v", err)
	}
	if _, err := f.fetch(t, view.Share.Token, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("祖先被删后下载应 ErrNotFound，实得 %v", err)
	}
}

func TestCreateShareRejectsDeadNode(t *testing.T) {
	f := newShareFixture(t)
	dir := f.mkdir(t, nil, "gone")
	if err := f.files.Delete(f.ctx, f.owner, dir.ID); err != nil {
		t.Fatalf("删目录: %v", err)
	}
	if _, err := f.shares.Create(f.ctx, ShareInput{OwnerID: f.owner, NodeID: dir.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("分享一个已删的节点应 ErrNotFound，实得 %v", err)
	}
}

func TestShareRevokeIsOwnerScoped(t *testing.T) {
	f := newShareFixture(t)
	other := f.newUser(t)
	file := f.upload(t, nil, "revoke.txt", "BYE")
	view := f.share(t, f.owner, file.ID)

	if err := f.shares.Revoke(f.ctx, other, view.Share.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("撤销别人的分享应 ErrNotFound，实得 %v", err)
	}
	if _, err := f.shares.Resolve(f.ctx, view.Share.Token); err != nil {
		t.Fatalf("别人的撤销不该动到这条分享: %v", err)
	}

	if err := f.shares.Revoke(f.ctx, f.owner, view.Share.ID); err != nil {
		t.Fatalf("自己撤销: %v", err)
	}
	if _, err := f.shares.Resolve(f.ctx, view.Share.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("撤销后 Resolve 应 ErrNotFound，实得 %v", err)
	}
	if err := f.shares.Revoke(f.ctx, f.owner, view.Share.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复撤销应 ErrNotFound，实得 %v", err)
	}
}

func TestShareExpiredIsNotFound(t *testing.T) {
	f := newShareFixture(t)
	file := f.upload(t, nil, "expired.txt", "OLD")

	past := time.Now().Add(-time.Minute)
	token := "tok-" + uuid.NewString()
	if _, err := f.store.CreateShare(f.ctx, file.ID, f.owner, token, &past); err != nil {
		t.Fatalf("直接插一条过期的分享: %v", err)
	}

	if _, err := f.shares.Resolve(f.ctx, token); !errors.Is(err, ErrNotFound) {
		t.Errorf("过期后 Resolve 应 ErrNotFound，实得 %v", err)
	}
	if _, err := f.fetch(t, token, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("过期后下载应 ErrNotFound，实得 %v", err)
	}
}
