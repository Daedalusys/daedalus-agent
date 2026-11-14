package rollback

import (
	"errors"
	"testing"
	"time"
)

// 正常路径:签发的 token 能验过,而且不早不晚。
func TestToken_IssueVerify(t *testing.T) {
	m := NewTokenManager([]byte("test-key-0123456789"))
	now := time.Unix(1_700_000_000, 0)

	tok, err := m.Issue("plan-a", "intent-1", now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("Issue returned an empty token")
	}
	if err := m.Verify(tok, "plan-a", "intent-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TTL 15 分钟:过期必须拒绝,而且拒绝原因要能分辨(控制面靠它写审计)。
func TestToken_RejectsExpired(t *testing.T) {
	m := NewTokenManager([]byte("test-key-0123456789"))
	now := time.Unix(1_700_000_000, 0)

	tok, err := m.Issue("plan-a", "intent-1", now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := m.Verify(tok, "plan-a", "intent-1", now.Add(16*time.Minute)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("got %v, want ErrTokenExpired", err)
	}
}

// token 绑定 plan_hash:admin 改过的计划不能拿旧 token 执行。
func TestToken_RejectsWrongPlanHash(t *testing.T) {
	m := NewTokenManager([]byte("test-key-0123456789"))
	now := time.Unix(1_700_000_000, 0)

	tok, err := m.Issue("plan-a", "intent-1", now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := m.Verify(tok, "plan-b", "intent-1", now); !errors.Is(err, ErrTokenPlanMismatch) {
		t.Fatalf("got %v, want ErrTokenPlanMismatch", err)
	}
}

// token 也绑定 intent_id:拿 A 的 token 去回滚 B 必须拒绝。
func TestToken_RejectsWrongIntent(t *testing.T) {
	m := NewTokenManager([]byte("test-key-0123456789"))
	now := time.Unix(1_700_000_000, 0)

	tok, err := m.Issue("plan-a", "intent-1", now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := m.Verify(tok, "plan-a", "intent-2", now); !errors.Is(err, ErrTokenIntentMismatch) {
		t.Fatalf("got %v, want ErrTokenIntentMismatch", err)
	}
}

// 单次使用:同一个 token 第二次 APPLY 必须被拒。
func TestToken_SingleUse(t *testing.T) {
	m := NewTokenManager([]byte("test-key-0123456789"))
	now := time.Unix(1_700_000_000, 0)

	tok, err := m.Issue("plan-a", "intent-1", now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := m.Verify(tok, "plan-a", "intent-1", now); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if err := m.Verify(tok, "plan-a", "intent-1", now); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("second Verify: got %v, want ErrTokenUsed", err)
	}
}

// 进程重启后旧 token 失效:新进程(同一把磁盘 key)必须不认上一个进程
// 签发的 token —— 要求重新 PLAN。
func TestToken_InvalidAfterRestart(t *testing.T) {
	key := []byte("test-key-0123456789")
	first := NewTokenManager(key)
	now := time.Unix(1_700_000_000, 0)

	tok, err := first.Issue("plan-a", "intent-1", now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	second := NewTokenManager(key) // 重启后的新进程
	if err := second.Verify(tok, "plan-a", "intent-1", now); !errors.Is(err, ErrTokenUnknown) {
		t.Fatalf("got %v, want ErrTokenUnknown", err)
	}
}

// 空 key 是装配错误:宁可签发失败,也不要签出谁也验不了的 token。
func TestToken_RejectsEmptyKey(t *testing.T) {
	for _, key := range [][]byte{nil, {}} {
		m := NewTokenManager(key)
		if _, err := m.Issue("plan-a", "intent-1", time.Now()); err == nil {
			t.Fatal("Issue with empty key = nil error, want error")
		}
	}
}
