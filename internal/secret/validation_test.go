// T7 Step 5 — 真机 (real-machine) end-to-end validation of
// the secret transaction cache.
//
// Why a separate file: the brief's Step 5 wants a real exercise
// of the security-critical cache semantics
// (transaction-scoped + panic-recovery + auto-clear) on a real
// in-process cache. There is no filesystem here (spec N6:
// no SecretProvider integration, no file I/O), so "real" means
// a real goroutine context with real panic propagation.
//
// Run with -v to capture the dumped cache state:
//
//	go test -race -count=1 -run TestValidation ./internal/secret/ -v
//
// Captured output goes into the T7 task report.
package secret_test

import (
	"errors"
	"testing"

	"github.com/Daedalusys/daedalus-agent/internal/secret"
)

// TestValidation_NormalExitClears exercises the happy path: a
// transaction stores a credential, "uses" it inside the txn,
// and the cache MUST be empty after WithTransaction returns.
// This is the central contract of the cache.
func TestValidation_NormalExitClears(t *testing.T) {
	var c secret.Cache
	c.Set("bearer", "production-bearer-token-abc123")

	err := c.WithTransaction(func() error {
		// Inside the transaction: credential is visible.
		v, ok := c.Get("bearer")
		if !ok || v != "production-bearer-token-abc123" {
			t.Errorf("inside tx: Get returned (%q, %v); want visible credential",
				v, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}

	// After WithTransaction returns the cache is empty.
	v, ok := c.Get("bearer")
	if ok {
		t.Errorf("post-tx Get: (%q, %v); cache not cleared", v, ok)
	}
	t.Logf("after normal exit: cache empty (Get returned (%q, %v))", v, ok)
}

// TestValidation_PanicClearsAndReturnsError exercises the
// security-critical panic path: a transaction that panics MUST
// leave the cache empty (no leaked credential) AND the panic
// MUST be recovered as an error (not re-raised — the caller is
// not on the same goroutine as the test's defer chain).
func TestValidation_PanicClearsAndReturnsError(t *testing.T) {
	var c secret.Cache
	c.Set("bearer", "production-bearer-token-panic-xyz")

	err := c.WithTransaction(func() error {
		// Inside the transaction: credential is visible.
		v, ok := c.Get("bearer")
		if !ok || v != "production-bearer-token-panic-xyz" {
			t.Errorf("inside tx: Get returned (%q, %v); want visible credential",
				v, ok)
		}
		panic("simulated credentialed operation panicked")
	})
	if err == nil {
		t.Fatal("WithTransaction returned nil err after panic; want error")
	}
	if !containsSubstr(err.Error(), "simulated credentialed operation panicked") {
		t.Errorf("error message %q does not mention panic value", err.Error())
	}

	// After panic + WithTransaction returns: cache is empty.
	v, ok := c.Get("bearer")
	if ok {
		t.Errorf("post-panic Get: (%q, %v); cache not cleared — credential leaked",
			v, ok)
	}
	t.Logf("after panic: cache empty (err = %q)", err.Error())
}

// TestValidation_FnErrorPropagates exercises the third path:
// fn returns a non-nil error (not a panic). The cache is still
// cleared (the contract holds for ALL exit shapes: return,
// error, panic).
func TestValidation_FnErrorPropagates(t *testing.T) {
	var c secret.Cache
	c.Set("bearer", "tmp-bearer")

	sentinel := errors.New("auth backend 503")
	err := c.WithTransaction(func() error {
		return sentinel
	})
	if err == nil {
		t.Fatal("WithTransaction returned nil err; want propagated sentinel")
	}
	if err != sentinel {
		t.Errorf("err = %v; want %v", err, sentinel)
	}
	v, ok := c.Get("bearer")
	if ok {
		t.Errorf("post-error Get: (%q, %v); cache not cleared after fn error", v, ok)
	}
	t.Logf("after fn error: cache empty (err = %q)", err.Error())
}

// containsSubstr is a tiny strings.Contains replacement to
// keep the import set small (no "strings" needed). The
// package-level helper named `contains` in cache_test.go is
// intentionally not reused to keep this file standalone.
func containsSubstr(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
