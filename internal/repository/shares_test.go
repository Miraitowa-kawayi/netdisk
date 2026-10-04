package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// 分享链接的持久化契约。打真库（先 `make up`），库不可达时 Skip —— SKIP ≠ 过。
//
// 最要紧的一条是 NodeIsLive：它决定"分享链接什么时候算死了"。
// shares.node_id 的 ON DELETE CASCADE 对弱删无效，所以这条查询是唯一的防线。
// ---------------------------------------------------------------------------

func TestNodeIsLive(t *testing.T) {
	st, ctx := newTestStore(t)
	fx := newFixture(t, st, ctx) // root/A/B/C, root/D, root/E；B 在 A 下、C 在 B 下

	t.Run("活的节点", func(t *testing.T) {
		for name, id := range map[string]uuid.UUID{"A": fx.a, "B": fx.b, "C": fx.c, "E": fx.e} {
			live, err := st.NodeIsLive(ctx, fx.owner, id)
			if err != nil {
				t.Fatalf("NodeIsLive(%s): %v", name, err)
			}
			if !live {
				t.Errorf("%s 应该是活的", name)
			}
		}
	})

	t.Run("不存在的节点", func(t *testing.T) {
		live, err := st.NodeIsLive(ctx, fx.owner, uuid.New())
		if err != nil {
			t.Fatalf("NodeIsLive(随机): %v", err)
		}
		if live {
			t.Error("随机 uuid 不该是活的")
		}
	})

	t.Run("祖先被删 → 子孙也不活（这条是重点）", func(t *testing.T) {
		if err := st.SoftDeleteSubtree(ctx, fx.owner, fx.a); err != nil {
			t.Fatalf("软删 A: %v", err)
		}
		for name, id := range map[string]uuid.UUID{"A": fx.a, "B": fx.b, "C": fx.c} {
			live, err := st.NodeIsLive(ctx, fx.owner, id)
			if err != nil {
				t.Fatalf("NodeIsLive(%s): %v", name, err)
			}
			if live {
				t.Errorf("%s 在 A 被删后不该还活着", name)
			}
		}
		// 反向对照：没被动过的兄弟节点 E 必须仍然是活的，别把整棵树都判死。
		if live, err := st.NodeIsLive(ctx, fx.owner, fx.e); err != nil || !live {
			t.Errorf("E 不该受 A 的影响: live=%v err=%v", live, err)
		}
	})

	t.Run("别人看不到（owner 也要对）", func(t *testing.T) {
		other := newTestUser(t, st, ctx)
		live, err := st.NodeIsLive(ctx, other, fx.e)
		if err != nil {
			t.Fatalf("NodeIsLive(别的 owner): %v", err)
		}
		if live {
			t.Error("换个 owner 就不该查到别人的节点")
		}
	})
}

func TestShareCreateGetAndList(t *testing.T) {
	st, ctx := newTestStore(t)
	fx := newFixture(t, st, ctx)
	blob := newTestBlob(t, st, ctx)
	fileID := newTestFile(t, st, ctx, fx.owner, nil, "shared.bin", blob.ID)

	token := "tok-" + uuid.NewString()
	created, err := st.CreateShare(ctx, fileID, fx.owner, token, nil)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if created.ExpiresAt != nil {
		t.Errorf("传 nil 应该是永不过期，实得 %v", created.ExpiresAt)
	}

	got, err := st.GetShareByToken(ctx, token)
	if err != nil {
		t.Fatalf("GetShareByToken: %v", err)
	}
	if got.ID != created.ID || got.NodeID != fileID || got.CreatedBy != fx.owner {
		t.Errorf("按 token 取回的行对不上: %+v", got)
	}

	shares, err := st.ListSharesByOwner(ctx, fx.owner)
	if err != nil {
		t.Fatalf("ListSharesByOwner: %v", err)
	}
	found := false
	for _, s := range shares {
		if s.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Error("刚建的分享应该出现在列表里")
	}

	if _, err := st.GetShareByToken(ctx, "tok-does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("取不存在的 token 应 ErrNotFound，实得 %v", err)
	}
}

func TestShareDeleteIsOwnerScoped(t *testing.T) {
	st, ctx := newTestStore(t)
	fx := newFixture(t, st, ctx)
	blob := newTestBlob(t, st, ctx)
	fileID := newTestFile(t, st, ctx, fx.owner, nil, "owned.bin", blob.ID)
	other := newTestUser(t, st, ctx)

	token := "tok-" + uuid.NewString()
	share, err := st.CreateShare(ctx, fileID, fx.owner, token, nil)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	// 别人撤销：必须 ErrNotFound，而且那行要还在。
	if err := st.DeleteShare(ctx, other, share.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("撤销别人的分享应 ErrNotFound，实得 %v", err)
	}
	if _, err := st.GetShareByToken(ctx, token); err != nil {
		t.Fatalf("别人的撤销不该动到这条分享: %v", err)
	}

	// 自己撤销：成功，且 token 立刻失效。
	if err := st.DeleteShare(ctx, fx.owner, share.ID); err != nil {
		t.Fatalf("自己撤销: %v", err)
	}
	if _, err := st.GetShareByToken(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Errorf("撤销后 token 应失效，实得 %v", err)
	}
}

func TestIncrementShareVisit(t *testing.T) {
	st, ctx := newTestStore(t)
	fx := newFixture(t, st, ctx)
	blob := newTestBlob(t, st, ctx)
	fileID := newTestFile(t, st, ctx, fx.owner, nil, "counted.bin", blob.ID)

	token := "tok-" + uuid.NewString()
	share, err := st.CreateShare(ctx, fileID, fx.owner, token, nil)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := st.IncrementShareVisit(ctx, share.ID); err != nil {
			t.Fatalf("第 %d 次加计数: %v", i+1, err)
		}
	}
	got, err := st.GetShareByToken(ctx, token)
	if err != nil {
		t.Fatalf("GetShareByToken: %v", err)
	}
	if got.VisitCount != 3 {
		t.Errorf("visit_count = %d, want 3", got.VisitCount)
	}
}

func TestCreateShareWithExpiry(t *testing.T) {
	st, ctx := newTestStore(t)
	fx := newFixture(t, st, ctx)
	blob := newTestBlob(t, st, ctx)
	fileID := newTestFile(t, st, ctx, fx.owner, nil, "expiring.bin", blob.ID)

	at := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	share, err := st.CreateShare(ctx, fileID, fx.owner, "tok-"+uuid.NewString(), &at)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if share.ExpiresAt == nil || !share.ExpiresAt.Equal(at) {
		t.Errorf("expires_at = %v, want %v", share.ExpiresAt, at)
	}
}
