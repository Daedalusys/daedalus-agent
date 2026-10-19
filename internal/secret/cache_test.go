// Tests for the secret transaction cache.
//
// The contract (spec §N6 + plan §T7):
//
//   - A credential is stored in the cache for the duration of
//     a single transaction. The transaction wrapper
//     (WithTransaction) clears the cache on both normal
//     return AND panic, so a process crash never leaves the
//     credential in memory long enough to be exfiltrated
//     via /proc/<pid>/mem.
//
//   - No file I/O (no /tmp, no /var/run/secrets, no disk
//     persistence of any kind). The credential lives only
//     in the process address space.
//
//   - No SecretProvider integration. The cache is a
//     minimal in-memory key/value store; the agent does
//     not call out to the (future) SecretProvider
//     service. A future task wires the two together.
//
// The tests assert the contract mechanically — no
// mocking beyond what the language gives us, no "test
// the test" indirection.
package secret_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Daedalusys/daedalus-agent/internal/secret"
)

// TestCache_GetOnEmpty_ReturnsMiss asserts the empty-cache
// miss: Get on a never-set key returns ("", false).
func TestCache_GetOnEmpty_ReturnsMiss(t *testing.T) {
	var c secret.Cache
	if v, ok := c.Get("k"); ok {
		t.Errorf("Get on empty cache: (%q, %v); want (\"\", false)", v, ok)
	}
}

// TestCache_SetThenGet asserts the happy path: Set then Get
// returns the value.
func TestCache_SetThenGet(t *testing.T) {
	var c secret.Cache
	c.Set("k", "s3cr3t")
	if v, ok := c.Get("k"); !ok || v != "s3cr3t" {
		t.Errorf("Get after Set: (%q, %v); want (\"s3cr3t\", true)", v, ok)
	}
}

// TestCache_SetOverwrites asserts Set on an existing key
// overwrites the value (transactional re-binding pattern).
func TestCache_SetOverwrites(t *testing.T) {
	var c secret.Cache
	c.Set("k", "v1")
	c.Set("k", "v2")
	if v, _ := c.Get("k"); v != "v2" {
		t.Errorf("Get after overwrite: %q; want v2", v)
	}
}

// TestCache_Clear_RemovesAll asserts Clear empties the cache.
func TestCache_Clear_RemovesAll(t *testing.T) {
	var c secret.Cache
	c.Set("k1", "v1")
	c.Set("k2", "v2")
	c.Clear()
	if v, ok := c.Get("k1"); ok {
		t.Errorf("Get(k1) after Clear: (%q, %v); want (\"\", false)", v, ok)
	}
	if v, ok := c.Get("k2"); ok {
		t.Errorf("Get(k2) after Clear: (%q, %v); want (\"\", false)", v, ok)
	}
}

// TestCache_WithTransaction_NormalExitClears is the central
// contract: a non-panicking transaction MUST leave the cache
// empty when it returns. The credential used inside the
// transaction is gone by the time WithTransaction returns.
func TestCache_WithTransaction_NormalExitClears(t *testing.T) {
	var c secret.Cache
	c.Set("token", "secret-bearer-12345")

	err := c.WithTransaction(func() error {
		// Inside the transaction the credential IS visible.
		if v, _ := c.Get("token"); v != "secret-bearer-12345" {
			t.Errorf("inside tx: Get returned %q; want secret-bearer-12345", v)
		}
		return nil
	})
	if err != nil {
		t.Errorf("WithTransaction returned %v; want nil", err)
	}
	// After the transaction, the cache MUST be empty.
	if v, ok := c.Get("token"); ok {
		t.Errorf("post-tx Get: (%q, %v); want (\"\", false) — cache not cleared", v, ok)
	}
}

// TestCache_WithTransaction_FnErrorPropagates asserts that
// the function's error is returned to the caller (NOT
// swallowed), AND the cache is still cleared.
func TestCache_WithTransaction_FnErrorPropagates(t *testing.T) {
	var c secret.Cache
	c.Set("token", "secret")

	sentinel := errors.New("auth failed")
	err := c.WithTransaction(func() error {
		_, _ = c.Get("token") // pretend to use
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("WithTransaction returned %v; want %v", err, sentinel)
	}
	if v, ok := c.Get("token"); ok {
		t.Errorf("post-error Get: (%q, %v); cache not cleared after fn error", v, ok)
	}
}

// TestCache_WithTransaction_PanicClearsAndReturnsError is
// the security-critical contract: if the transaction
// function panics, the cache MUST be cleared (no leaked
// credential) AND the panic is recovered as an error
// (not re-raised, since the caller is not on the same
// goroutine as the test's defer chain).
//
// The credential never escapes the transaction scope even
// if the transaction fails abruptly.
func TestCache_WithTransaction_PanicClearsAndReturnsError(t *testing.T) {
	var c secret.Cache
	c.Set("token", "secret-panic-bearer")

	err := c.WithTransaction(func() error {
		_, _ = c.Get("token") // pretend to use
		panic("kaboom")
	})
	if err == nil {
		t.Fatal("WithTransaction returned nil error after panic; want error")
	}
	// Error message should mention the panic value so the
	// operator can triage.
	if !contains(err.Error(), "kaboom") {
		t.Errorf("error message %q does not mention panic value", err.Error())
	}
	// And the credential is GONE.
	if v, ok := c.Get("token"); ok {
		t.Errorf("post-panic Get: (%q, %v); cache not cleared after panic", v, ok)
	}
}

// TestCache_WithTransaction_EmptyCache is the trivial case:
// no values set, transaction does nothing, cache stays
// empty.
func TestCache_WithTransaction_EmptyCache(t *testing.T) {
	var c secret.Cache
	if err := c.WithTransaction(func() error { return nil }); err != nil {
		t.Errorf("WithTransaction(empty): %v; want nil", err)
	}
	if _, ok := c.Get("k"); ok {
		t.Error("post-tx Get on empty cache: ok; want false")
	}
}

// TestCache_WithTransaction_NestedForbidden: a panic inside
// the transaction does NOT cause a nested WithTransaction
// to be entered, because the defer-only Clear() runs
// inside the same defer frame. We test the simpler
// scenario: a transaction can be re-entered after the
// first one returns cleanly.
func TestCache_WithTransaction_Reentrant(t *testing.T) {
	var c secret.Cache
	c.Set("k1", "v1")
	if err := c.WithTransaction(func() error {
		_, _ = c.Get("k1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// First tx cleared the cache.
	if _, ok := c.Get("k1"); ok {
		t.Fatal("cache not cleared after first tx")
	}
	// Set fresh, run second tx.
	c.Set("k2", "v2")
	if err := c.WithTransaction(func() error {
		if v, _ := c.Get("k2"); v != "v2" {
			t.Errorf("second tx: Get returned %q; want v2", v)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("k2"); ok {
		t.Error("cache not cleared after second tx")
	}
}

// TestCache_Concurrent_SetGetClear races 8 goroutines doing
// mixed Set / Get / Clear. Run with -race to catch data
// races. The contract: no panic, no race, the final state
// is "either cleared or has the last Set value" (we don't
// assert exact count because of the race window).
func TestCache_Concurrent_SetGetClear(t *testing.T) {
	var c secret.Cache
	const goroutines = 8
	const ops = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				switch (g + i) % 3 {
				case 0:
					c.Set(fmt.Sprintf("k-%d-%d", g, i), "v")
				case 1:
					_, _ = c.Get(fmt.Sprintf("k-%d-%d", g, i))
				case 2:
					c.Clear()
				}
			}
		}()
	}
	wg.Wait()
	// No assertion on final state — the contract is "no
	// panic, no race, no data corruption". -race enforces
	// the data-race half; the panic half is enforced by
	// the test reaching this line.
}

// TestCache_Concurrent_WithTransaction races multiple
// concurrent WithTransaction calls. Each one MUST clear
// the cache when it returns, regardless of the others.
// We assert: at the end of all transactions, the cache
// is empty (last writer wins, and the last Clear is
// always the last transaction's defer).
func TestCache_Concurrent_WithTransaction(t *testing.T) {
	var c secret.Cache
	var wg sync.WaitGroup
	const goroutines = 16
	var panicCount atomic.Int32
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			// Half the goroutines panic, half return
			// cleanly. Both cases must clear.
			if g%2 == 0 {
				c.Set(fmt.Sprintf("k-%d", g), "v")
				_ = c.WithTransaction(func() error {
					_, _ = c.Get(fmt.Sprintf("k-%d", g))
					return nil
				})
			} else {
				c.Set(fmt.Sprintf("k-%d", g), "v")
				err := c.WithTransaction(func() error {
					_, _ = c.Get(fmt.Sprintf("k-%d", g))
					panic("intentional")
				})
				if err == nil {
					t.Error("WithTransaction(panic) returned nil err")
				} else {
					panicCount.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	// All panicking goroutines recovered.
	if panicCount.Load() != goroutines/2 {
		t.Errorf("panicCount = %d; want %d", panicCount.Load(), goroutines/2)
	}
	// Cache MUST be empty after all transactions.
	// (scan all keys by trying common ones)
	for i := 0; i < goroutines; i++ {
		if v, ok := c.Get(fmt.Sprintf("k-%d", i)); ok {
			t.Errorf("post-concurrent-tx Get(k-%d): (%q, %v); cache not cleared", i, v, ok)
		}
	}
}

// TestCache_ReuseAfterClear asserts the cache can be
// re-used after Clear: Set/Get/WithTransaction works
// exactly as on a fresh cache.
func TestCache_ReuseAfterClear(t *testing.T) {
	var c secret.Cache
	c.Set("k", "v1")
	c.Clear()
	c.Set("k", "v2")
	if v, _ := c.Get("k"); v != "v2" {
		t.Errorf("Get after Clear+Set: %q; want v2", v)
	}
	if err := c.WithTransaction(func() error {
		if v, _ := c.Get("k"); v != "v2" {
			t.Errorf("inside reused tx: Get returned %q; want v2", v)
		}
		return nil
	}); err != nil {
		t.Errorf("WithTransaction(reused): %v; want nil", err)
	}
	if _, ok := c.Get("k"); ok {
		t.Error("post-reuse-tx Get: ok; cache not cleared")
	}
}

// contains is a tiny strings.Contains replacement to keep
// the import set small (no "strings" package needed).
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
