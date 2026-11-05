// policy_hash.go owns the agent's active-policy file digest.
package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// policyHash returns the sha256 digest (lowercase hex) of the bytes at
// path.
//
// Three states, deliberately distinct on the wire:
//
//   - path == ""           → ("", nil): no policy configured. This is a
//     supported development shape, not an error.
//   - path readable        → (64-char lowercase hex, nil).
//   - path set, unreadable → ("", err): a deployment accident — the
//     operator configured a policy path that is absent or unreadable.
//     The caller turns this into FailedPrecondition rather than silently
//     reporting "no policy", so ops can tell "not configured" apart from
//     "policy went missing".
func policyHash(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read policy %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
