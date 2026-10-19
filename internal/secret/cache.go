// Package secret owns the daedalus-agent in-process secret
// cache.
//
// Why a package and not a struct in the rpc package: the
// cache is consumed by the tx state machine (a future
// pass-through for credentialed operations), the
// rollback backend (if it ever needs to authenticate
// against the snapshot store), and the RPC handlers
// (when an intent arrives with a credential attached).
// A package gives us a single point of policy: where
// secrets live, when they are released, and what code
// paths can read them.
//
// Security policy (spec N6 + plan §T7):
//
//   - Transaction-scoped: a secret is stored at the
//     start of a logical operation and released at the
//     end. Releasing is automatic — the WithTransaction
//     wrapper runs defer Clear() so even a panic
//     releases the secret. The caller does not need to
//     remember to clear.
//
//   - Process-local: the cache lives only in this
//     process's address space. There is no /tmp file,
//     no /var/run/secrets bind mount, no disk
//     persistence of any kind. A process crash
//     destroys the cache; the secret never reaches the
//     filesystem.
//
//   - No SecretProvider integration (P0.5 scope). The
//     cache is a stand-alone key/value store. A future
//     task wires the cache to the SecretProvider
//     service for fetch / rotate; for P0.5 the agent
//     receives credentials over the wire (in a future
//     T9+) and stashes them via WithTransaction.
package secret

import (
	"fmt"
	"sync"
)

// Cache is a thread-safe in-memory key/value store for
// short-lived secrets. Zero value is ready to use; no
// constructor needed.
//
// Backing storage is sync.Map, which gives us lock-free
// reads in the common case (a single transaction's reads
// do not contend with each other) and proper serialisation
// for writes. Clear() uses Range to delete every entry,
// which is O(n) in the number of entries — fine for the
// small handful of secrets a single transaction holds.
type Cache struct {
	m sync.Map
}

// Get returns the value stored under key, or ("", false)
// if absent. The bool return mirrors sync.Map.Load so
// callers can distinguish "not present" from "present
// and empty string" (the latter is rare but possible).
func (c *Cache) Get(key string) (string, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// Set stores value under key, overwriting any previous
// value. Use WithTransaction to ensure Set-then-use-then-clear
// atomicity; Set alone is for cases where the caller manages
// the lifetime explicitly (e.g. a test fixture).
func (c *Cache) Set(key, value string) {
	c.m.Store(key, value)
}

// Clear removes every entry. O(n) in the number of entries.
// Called automatically by WithTransaction; rarely needed
// elsewhere.
func (c *Cache) Clear() {
	c.m.Range(func(k, _ any) bool {
		c.m.Delete(k)
		return true
	})
}

// WithTransaction runs fn with the cache, then clears the
// cache. The clear runs even if fn panics — this is the
// security-critical guarantee: a panic in a credentialed
// operation must not leave the secret in memory past the
// transaction scope.
//
// The recovered panic is returned as an error (NOT
// re-raised), so callers using errgroup / standard error
// handling see the failure. The error message includes
// the panic value to aid triage.
//
// fn's error is returned to the caller verbatim. If fn
// returns nil and does not panic, WithTransaction returns
// nil after clearing.
//
// Typical use:
//
//	secret := fetchFromWire()       // future T9+
//	if err := c.WithTransaction(func() error {
//	    c.Set("bearer", secret)
//	    return doWork()
//	}); err != nil {
//	    // cache is already empty here
//	    return err
//	}
func (c *Cache) WithTransaction(fn func() error) (err error) {
	defer func() {
		c.Clear()
		if r := recover(); r != nil {
			err = fmt.Errorf("secret cache: panic in transaction: %v", r)
		}
	}()
	return fn()
}
