package repository

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// 测试脚手架
//
// 这些用例打的是**真数据库** —— 递归 CTE 这种东西，假一个出来测不出什么。
// 库连不上就 Skip，让 `go test ./...` 在没有数据库的环境里也能跑完。
//
// 每个用例建一个自己的用户（用户名带随机后缀），t.Cleanup 里删掉它：
// users → nodes 是 ON DELETE CASCADE，于是整棵测试树跟着一起清干净，
// 不会在开发库里留垃圾。
// ---------------------------------------------------------------------------

const fallbackTestDSN = "postgres://netdisk:netdisk@localhost:5433/netdisk?sslmode=disable"

func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = fallbackTestDSN
	}
	ctx := context.Background()

	st, err := New(ctx, dsn)
	if err != nil {
		t.Skipf("跳过：建连接池失败（%v）", err)
	}
	if err := st.Ping(ctx); err != nil {
		st.Close()
		t.Skipf("跳过：数据库不可达（%v）—— 先跑 `make up`", err)
	}
	t.Cleanup(st.Close)
	return st, ctx
}

func newTestUser(t *testing.T, st *Store, ctx context.Context) uuid.UUID {
	t.Helper()
	u, err := st.CreateUser(ctx, "test-"+uuid.NewString(), "not-a-real-bcrypt-hash", "")
	if err != nil {
		t.Fatalf("建测试用户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u.ID
}

// fixture 是 owner 之下的一棵树：
//
//	root/
//	  A/
//	    B/
//	      C/
//	    D/
//	  E/
type fixture struct {
	owner         uuid.UUID
	a, b, c, d, e uuid.UUID
}

func newFixture(t *testing.T, st *Store, ctx context.Context) fixture {
	t.Helper()
	owner := newTestUser(t, st, ctx)

	mkdir := func(parent *uuid.UUID, name string) uuid.UUID {
		n, err := st.CreateNode(ctx, owner, parent, name, true, 0, nil)
		if err != nil {
			t.Fatalf("建目录 %s: %v", name, err)
		}
		return n.ID
	}

	f := fixture{owner: owner}
	f.a = mkdir(nil, "A")
	f.b = mkdir(&f.a, "B")
	f.c = mkdir(&f.b, "C")
	f.d = mkdir(&f.a, "D")
	f.e = mkdir(nil, "E")
	return f
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }

// ---------------------------------------------------------------------------
// D2 的契约：环检测
// 实现 repository/nodes.go 的 IsSelfOrDescendant 之前，这一组是红的。
// ---------------------------------------------------------------------------

func TestIsSelfOrDescendant(t *testing.T) {
	st, ctx := newTestStore(t)
	f := newFixture(t, st, ctx)

	cases := []struct {
		name      string
		candidate uuid.UUID
		ancestor  uuid.UUID
		want      bool
	}{
		{"自己算自己的子孙", f.a, f.a, true},
		{"直接子目录", f.b, f.a, true},
		{"孙目录", f.c, f.a, true},
		{"另一个子目录", f.d, f.a, true},
		{"根目录下无关的目录", f.e, f.a, false},
		{"方向不能反：A 不是 B 的子孙", f.a, f.b, false},
		{"方向不能反：E 不是 B 的子孙", f.e, f.b, false},
	}
	for _, tc := range cases {
		got, err := st.IsSelfOrDescendant(ctx, f.owner, tc.candidate, tc.ancestor)
		if err != nil {
			t.Fatalf("%s: 不该返回错误，实得 %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: IsSelfOrDescendant = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsSelfOrDescendantUnknownNodeIsFalse(t *testing.T) {
	st, ctx := newTestStore(t)
	f := newFixture(t, st, ctx)
	missing := uuid.New()

	cases := []struct {
		name      string
		candidate uuid.UUID
		ancestor  uuid.UUID
	}{
		{"candidate 不存在", missing, f.a},
		{"ancestor 不存在", f.a, missing},
	}
	for _, tc := range cases {
		got, err := st.IsSelfOrDescendant(ctx, f.owner, tc.candidate, tc.ancestor)
		if err != nil {
			t.Errorf("%s: 不该返回错误，实得 %v", tc.name, err)
			continue
		}
		if got {
			t.Errorf("%s: 应为 false", tc.name)
		}
	}
}

func TestIsSelfOrDescendantOtherOwnerIsFalse(t *testing.T) {
	st, ctx := newTestStore(t)
	f := newFixture(t, st, ctx)

	// 另一个用户下的同名目录，不能被当成"我的子孙"。
	other := newTestUser(t, st, ctx)
	otherNode, err := st.CreateNode(ctx, other, nil, "A", true, 0, nil)
	if err != nil {
		t.Fatalf("建别人的目录: %v", err)
	}

	got, err := st.IsSelfOrDescendant(ctx, f.owner, otherNode.ID, f.a)
	if err != nil {
		t.Fatalf("IsSelfOrDescendant: %v", err)
	}
	if got {
		t.Error("别人家的节点不是我的子孙")
	}
}

func TestIsSelfOrDescendantDeletedNodeIsFalse(t *testing.T) {
	st, ctx := newTestStore(t)
	f := newFixture(t, st, ctx)

	if err := st.SoftDeleteSubtree(ctx, f.owner, f.a); err != nil {
		t.Fatalf("SoftDeleteSubtree: %v", err)
	}

	// A 整棵被弱删之后，C 不该再被算成 A 的子孙。
	got, err := st.IsSelfOrDescendant(ctx, f.owner, f.c, f.a)
	if err != nil {
		t.Fatalf("IsSelfOrDescendant: %v", err)
	}
	if got {
		t.Error("已删除的节点不该参与判断")
	}
}

func TestSoftDeleteSubtreeCoversWholeTree(t *testing.T) {
	st, ctx := newTestStore(t)
	f := newFixture(t, st, ctx)

	if err := st.SoftDeleteSubtree(ctx, f.owner, f.a); err != nil {
		t.Fatalf("SoftDeleteSubtree: %v", err)
	}

	for name, id := range map[string]uuid.UUID{"A": f.a, "B": f.b, "C": f.c, "D": f.d} {
		if _, err := st.GetNode(ctx, f.owner, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s 应该已被弱删，GetNode = %v", name, err)
		}
	}
	if _, err := st.GetNode(ctx, f.owner, f.e); err != nil {
		t.Errorf("E 不该被牵连: %v", err)
	}

	kids, err := st.ListChildren(ctx, f.owner, nil)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(kids) != 1 || kids[0].ID != f.e {
		t.Errorf("根目录应只剩 E，实得 %d 项", len(kids))
	}
}

func TestSoftDeleteSubtreeMissingNode(t *testing.T) {
	st, ctx := newTestStore(t)
	f := newFixture(t, st, ctx)

	if err := st.SoftDeleteSubtree(ctx, f.owner, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("删不存在的节点 = %v, want ErrNotFound", err)
	}
}

func TestUpdateNode(t *testing.T) {
	st, ctx := newTestStore(t)
	f := newFixture(t, st, ctx)

	// 移动：把 C 移到根目录（parentID 传 nil，但 setParent 为 true）
	n, err := st.UpdateNode(ctx, f.owner, f.c, nil, nil, true)
	if err != nil {
		t.Fatalf("移到根目录: %v", err)
	}
	if n.ParentID != nil {
		t.Errorf("移到根目录后 parent_id 应为 NULL，实得 %v", *n.ParentID)
	}

	// 只改名：父目录必须原地不动
	newName := "C2"
	if n, err = st.UpdateNode(ctx, f.owner, f.c, &newName, nil, false); err != nil {
		t.Fatalf("改名: %v", err)
	}
	if n.Name != "C2" || n.ParentID != nil {
		t.Errorf("改名结果不对: name=%q parent=%v", n.Name, n.ParentID)
	}

	// 改名 + 移动一起做
	newName = "D2"
	if n, err = st.UpdateNode(ctx, f.owner, f.d, &newName, ptr(f.e), true); err != nil {
		t.Fatalf("改名+移动: %v", err)
	}
	if n.Name != "D2" || n.ParentID == nil || *n.ParentID != f.e {
		t.Errorf("改名+移动结果不对: name=%q parent=%v", n.Name, n.ParentID)
	}

	// 同层重名：根目录上已经有 E 了
	dup := "E"
	if _, err := st.UpdateNode(ctx, f.owner, f.c, &dup, nil, false); !errors.Is(err, ErrUniqueViolation) {
		t.Errorf("同层重名 = %v, want ErrUniqueViolation", err)
	}

	// 节点不存在
	if _, err := st.UpdateNode(ctx, f.owner, uuid.New(), nil, nil, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的节点 = %v, want ErrNotFound", err)
	}
}
