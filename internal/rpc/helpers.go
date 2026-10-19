// helpers.go — small shared utilities for the rpc package.
//
// Kept in its own file (rather than tacked onto agent_service.go)
// because the helpers have nothing to do with the agentService
// struct or its wiring: they are pure functions used by one or
// more handlers. Splitting them out keeps agent_service.go
// focused on struct composition.
package rpc

import (
	"crypto/rand"
	"encoding/hex"
)

// randomHex returns a hex string of length 2*n, drawn from n
// bytes of crypto/rand. Used to mint synthetic IDs for
// snapshot_id (snapshot_request.go) — see T8 review M3.
//
// On the (theoretically impossible on linux/darwin) rand.Read
// failure we return "deadbeef" rather than panicking: the
// caller is generating an ephemeral identifier, not a security
// primitive, and a deterministic-but-unique-per-call fallback
// is acceptable here. crypto/rand.Read failure in production
// would already be a host-level incident worth triaging; we do
// not want a per-RPC panic to mask the underlying cause.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "deadbeef"
	}
	return hex.EncodeToString(buf)
}
