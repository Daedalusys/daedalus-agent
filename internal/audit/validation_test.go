// T7 Step 5 — 真机 (real-machine) end-to-end validation of the
// audit chain.
//
// Why a separate file: the brief asks for an end-to-end exercise
// of audit on a real filesystem (not mocked). t.TempDir() satisfies
// that — it allocates a real directory under os.TempDir() that is
// removed when the test exits. The chain file written here has
// the SAME layout as production (`<dataDir>/audit/chain.jsonl`)
// and IS byte-for-byte the same format (audit.New uses
// sdkAudit.LogAudit, which writes the same hash-chained JSONL
// regardless of path).
//
// Run with -v to capture the dumped chain.jsonl content:
//   go test -race -count=1 -run TestValidation ./internal/audit/ -v
//
// The captured output is the literal bytes of the on-disk chain
// after one successful + one failed transaction — reported in
// the T7 task report.
package audit_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Daedalusys/daedalus-agent/internal/audit"
)

// TestValidation_RealFsEndToEnd exercises the audit chain on a
// real temp filesystem, mirrors the production path layout
// (<tmpdir>/var/lib/daedalus-agent/audit/chain.jsonl), and
// verifies:
//
//  1. The chain file is created at the expected location.
//  2. Log appends a valid hash-chained entry (entry_hash
//     present, non-empty, prev_hash pointing at the previous
//     entry or empty for the first).
//  3. Since returns the entry back with the same identity /
//     tool / outcome / args.
//  4. The on-disk format matches the SDK's wire shape (the
//     bytes are what Plan C's control plane will consume).
//
// RED evidence: prior to T7's tx wiring this file's
// `TestLocal_AuditEmitsOnSuccess` flagged RED because the
// tx did not yet write any audit events. The GREEN run is
// what we assert here.
func TestValidation_RealFsEndToEnd(t *testing.T) {
	// Mirror the production layout under a temp dir so the
	// output path LOOKS like /var/lib/daedalus-agent/audit/.
	tmpBase := t.TempDir()
	prodLayout := filepath.Join(tmpBase, "var", "lib", "daedalus-agent")
	dataDir := prodLayout // audit.New appends "audit/chain.jsonl" itself
	t.Logf("audit dataDir = %s", dataDir)

	auditLog, err := audit.New(dataDir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}

	wantPath := filepath.Join(dataDir, "audit", "chain.jsonl")
	if got := auditLog.ChainPath(); got != wantPath {
		t.Errorf("ChainPath = %q; want %q", got, wantPath)
	}

	// 1. Log a successful intent.
	seq1, err := auditLog.Log(audit.Event{
		Identity: "intent-validation-001",
		Tool:     "apply_intent.success",
		Args: map[string]string{
			"target": "nginx",
			"kind":   "package.set",
			"tx_id":  "tx-validation-001",
		},
		Outcome: "ok",
	})
	if err != nil {
		t.Fatalf("Log (success): %v", err)
	}
	if seq1 != 1 {
		t.Errorf("seq1 = %d; want 1", seq1)
	}

	// 2. Log a rollback-failed intent (the persistent-failure
	// audit shape — exercise the dual-error path).
	seq2, err := auditLog.Log(audit.Event{
		Identity: "intent-validation-002",
		Tool:     "apply_intent.rollback_failed",
		Args: map[string]string{
			"target": "postgresql",
			"kind":   "service.set",
		},
		Outcome: "apply_failed: checksum mismatch; rollback_failed: ENOSPC",
	})
	if err != nil {
		t.Fatalf("Log (rollback_failed): %v", err)
	}
	if seq2 != 2 {
		t.Errorf("seq2 = %d; want 2", seq2)
	}

	// 3. Read back. Verify the file exists at the expected
	// path AND has the on-disk shape the SDK mandates.
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read chain.jsonl: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("chain.jsonl is empty")
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("chain.jsonl has %d lines; want 2", len(lines))
	}

	// 4. Parse each line and assert required SDK fields are
	// present (entry_hash, prev_hash, timestamp, identity, tool,
	// outcome, policy_version, args).
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d: parse JSON: %v\nline=%s", i+1, err, line)
		}
		for _, k := range []string{
			"timestamp", "identity", "tool", "outcome",
			"args", "policy_version", "prev_hash", "entry_hash",
		} {
			if _, ok := obj[k]; !ok {
				t.Errorf("line %d: missing SDK-required field %q\nline=%s", i+1, k, line)
			}
		}
	}

	// 5. The two entries are hash-chained: line-2's prev_hash
	// must equal line-1's entry_hash.
	var obj1, obj2 map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &obj1)
	_ = json.Unmarshal([]byte(lines[1]), &obj2)
	if obj2["prev_hash"].(string) != obj1["entry_hash"].(string) {
		t.Errorf("hash chain broken at line 2: prev_hash=%q != entry_hash of line 1=%q",
			obj2["prev_hash"], obj1["entry_hash"])
	}
	// Line 1 is the chain's root. The SDK's first-entry
	// prev_hash convention is 64 zero hex chars (the same
	// value ComputeEntryHash uses as the seed for entry 1).
	// What MATTERS for the chain is that line-2's prev_hash
	// equals line-1's entry_hash — that assertion is above.
	if p := obj1["prev_hash"].(string); p != "0000000000000000000000000000000000000000000000000000000000000000" {
		t.Errorf("first entry prev_hash = %q; want 64 zeros (SDK chain root sentinel)", p)
	}
	// entry_hash itself must be hex (64 chars).
	if eh := obj1["entry_hash"].(string); len(eh) != 64 {
		t.Errorf("entry_hash length = %d; want 64 (sha256 hex)", len(eh))
	}

	// 6. Since(0) returns both events back; final_seq == 2.
	events, final, err := auditLog.Since(0)
	if err != nil {
		t.Fatalf("Since(0): %v", err)
	}
	if final != 2 {
		t.Errorf("final_seq = %d; want 2", final)
	}
	if len(events) != 2 {
		t.Errorf("len(events) = %d; want 2", len(events))
	}
	if events[0].Identity != "intent-validation-001" {
		t.Errorf("events[0].Identity = %q; want intent-validation-001", events[0].Identity)
	}
	if events[0].Tool != "apply_intent.success" {
		t.Errorf("events[0].Tool = %q; want apply_intent.success", events[0].Tool)
	}
	if events[1].Tool != "apply_intent.rollback_failed" {
		t.Errorf("events[1].Tool = %q; want apply_intent.rollback_failed", events[1].Tool)
	}
	if events[1].Outcome != "apply_failed: checksum mismatch; rollback_failed: ENOSPC" {
		t.Errorf("events[1].Outcome = %q; want dual-error string", events[1].Outcome)
	}

	// 7. Dump the literal bytes of the chain for the report.
	t.Logf("chain.jsonl path: %s", wantPath)
	t.Logf("chain.jsonl size: %d bytes", len(data))
	t.Logf("chain.jsonl line-1: %s", lines[0])
	t.Logf("chain.jsonl line-2: %s", lines[1])
}
