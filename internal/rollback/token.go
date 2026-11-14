package rollback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// tokenTTL 是 approve token 的有效期。
//
// 15 分钟:够人审一份计划,又短到让"签完忘了执行"的 token 自己作废。
const tokenTTL = 15 * time.Minute

// token 校验失败的原因。分成几个哨兵而不是一个笼统的错误:控制面要按原因
// 写审计(过期 / 哈希不符 / 已用过 对审查者是三件不同的事)。
var (
	// ErrTokenUnknown 表示这个 token 本进程从没签发过(含重启前的 token)。
	ErrTokenUnknown = errors.New("rollback: approve token unknown; plan again")
	// ErrTokenUsed 表示 token 已经被用过一次。
	ErrTokenUsed = errors.New("rollback: approve token already used")
	// ErrTokenExpired 表示 token 超过 TTL。
	ErrTokenExpired = errors.New("rollback: approve token expired")
	// ErrTokenPlanMismatch 表示 token 绑的是另一份计划。
	ErrTokenPlanMismatch = errors.New("rollback: approve token binds a different plan")
	// ErrTokenIntentMismatch 表示 token 绑的是另一个 intent。
	ErrTokenIntentMismatch = errors.New("rollback: approve token binds a different intent")
)

// tokenRecord 是一个已签发 token 的账目。
type tokenRecord struct {
	planHash string
	intentID string
	expiry   time.Time
	used     bool
}

// TokenManager 签发 / 校验一次性 approve token。
//
// 语义边界(别把它读大):token 只证明「这次 APPLY 绑定的就是那份 plan_hash」,
// **不**证明有人看过计划 —— 人在环路里由控制面保证。
//
// 为什么 token 不是纯无状态凭据:Verify 只拿到 token 字符串本身,不回查就
// 无从知道 expiry;账目放内存同时让「进程重启后旧 token 作废」成为自然结果
// (spec:重启后要求重新 PLAN)。
type TokenManager struct {
	key []byte

	mu     sync.Mutex
	issued map[string]tokenRecord
}

// NewTokenManager 用 HMAC key 构造。key 由调用方从磁盘读取/生成
// (<dataDir>/rollback_token.key,0600)。
func NewTokenManager(key []byte) *TokenManager {
	return &TokenManager{
		key:    append([]byte(nil), key...),
		issued: map[string]tokenRecord{},
	}
}

// Issue 为 (planHash, intentID) 签发一个 token,有效期 tokenTTL。
func (m *TokenManager) Issue(planHash, intentID string, now time.Time) (string, error) {
	if m == nil || len(m.key) == 0 {
		// 空 key 是装配错误:签出来的 token 谁也验不了,不如当场失败。
		return "", fmt.Errorf("rollback: approve token key is empty")
	}
	if planHash == "" || intentID == "" {
		return "", fmt.Errorf("rollback: approve token needs plan hash and intent id")
	}
	expiry := now.Add(tokenTTL)
	token := m.mac(planHash, intentID, expiry)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.issued == nil {
		m.issued = map[string]tokenRecord{}
	}
	m.issued[token] = tokenRecord{planHash: planHash, intentID: intentID, expiry: expiry}
	return token, nil
}

// Verify 校验 token 并把它标记为已用。
//
// 校验顺序有意为之:先查账目(不知道的 token 直接拒),再查是否用过,再查
// 过期,最后查绑定关系与 HMAC。任何一个失败都不消耗 token,只有全部通过才
// 标记 used —— 否则一次手误就能把合法 token 废掉。
func (m *TokenManager) Verify(token, planHash, intentID string, now time.Time) error {
	if m == nil || token == "" {
		return ErrTokenUnknown
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.issued[token]
	if !ok {
		return ErrTokenUnknown
	}
	if rec.used {
		return ErrTokenUsed
	}
	if now.After(rec.expiry) {
		return ErrTokenExpired
	}
	if rec.planHash != planHash {
		return ErrTokenPlanMismatch
	}
	if rec.intentID != intentID {
		return ErrTokenIntentMismatch
	}
	// 重算 HMAC:账目被改、或 key 换了,都在这里露出来。
	if !hmac.Equal([]byte(token), []byte(m.mac(rec.planHash, rec.intentID, rec.expiry))) {
		return ErrTokenUnknown
	}
	rec.used = true
	m.issued[token] = rec
	return nil
}

// mac 是 token 本体:base64url(HMAC-SHA256(key, plan_hash || intent_id || expiry))。
func (m *TokenManager) mac(planHash, intentID string, expiry time.Time) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(planHash))
	mac.Write([]byte("|"))
	mac.Write([]byte(intentID))
	mac.Write([]byte("|"))
	mac.Write([]byte(strconv.FormatInt(expiry.Unix(), 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
