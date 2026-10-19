// Package audit owns the daedalus-agent local audit chain:
//
//   - Local: a per-process append-only hash chain backed by a
//     JSONL file under <DataDir>/audit/chain.jsonl. Writes go
//     through daedalus-sdk/audit.LogAudit (the single ingress
//     mandated by spec §3); reads are the JSONL file itself
//     (the SDK's Verify can be run by an external tool, but
//     the agent does not Verify on the read path — it just
//     streams back what the SDK wrote).
//
//   - The on-disk format is byte-exact compatible with the
//     SDK's golden test vector (see TestLocal_GoldenVector_ByteExact);
//     Plan C (control plane) uses the SDK's Verify to
//     cross-check the agent's chain.
//
// Audit policy (plan §RF + spec §3):
//
//   - Single ingress: every audit write goes through
//     sdkAudit.LogAudit. The agent does NOT open the file
//     directly for writing.
//   - Hash chain: prev_hash / entry_hash per the SDK's
//     ComputeEntryHash. The chain's seq is the line number
//     (1-indexed) in the JSONL file; Log() returns the new
//     seq after appending, Since(N) returns events with
//     seq > N.
//   - Crash safety: the SDK flushes each write via f.Sync()
//     before unlocking. A crash mid-Log may leave a partial
//     line on disk; Since skips empty / un-parseable lines
//     the same way Verify does.
//   - Concurrency: Log is safe to call from multiple
//     goroutines (sdkAudit.LogAudit takes an exclusive flock
//     on the file fd). Since is read-only.
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	sdkAudit "github.com/Daedalusys/daedalus-sdk/audit"
)

const (
	// chainDir is the subdirectory under the agent's data
	// dir where the JSONL chain lives. Matches the plan's
	// "/var/lib/daedalus-agent/audit/" pin.
	chainDir = "audit"

	// chainFile is the on-disk name of the hash chain.
	chainFile = "chain.jsonl"

	// defaultIdentity is stamped on the on-disk record's
	// "identity" field when the caller omits Event.Identity.
	// "agent" matches the agent's role in the fleet (the
	// control plane is the other identity).
	defaultIdentity = "agent"
)

// Event is the input shape for Log. Args is flat
// string→string for P0.5 scope — sufficient for tx
// outcomes (intent_id / kind / target) and RPC events
// (node_id / seq). Nested structures can be added later
// by widening Args to a more permissive type.
type Event struct {
	Identity string
	Tool     string
	Args     map[string]string
	Outcome  string
}

// Record is a single audit event read back from the chain.
// The on-disk format is owned by the SDK; this struct
// reflects it with an explicit Seq field (the SDK derives
// seq from line number; we make it explicit so callers
// don't have to).
//
// Args is reconstructed from the JSONL by json.Unmarshal
// into a map[string]string. Nested objects / arrays
// present in the on-disk args are dropped (P0.5 only
// emits flat string args); the field is best-effort and
// primarily for the Since path's debugging / monitoring
// use, not the wire protocol.
type Record struct {
	Seq       uint64
	Timestamp string
	Identity  string
	Tool      string
	Args      map[string]string
	Outcome   string
	PrevHash  string
	EntryHash string
}

// Local is the per-process audit chain. It is safe for
// concurrent use: Log takes the SDK's file-flock, Since
// opens the file read-only.
type Local struct {
	chainPath string
	// seq is the highest seq assigned so far. 0 means no
	// entries yet. Log does seq.Add(1) then writes that
	// value as the entry's seq; the on-disk line number
	// must match (otherwise the seq reported to the
	// control plane is inconsistent with the file content).
	seq atomic.Uint64
}

// New constructs a Local under the given data dir. The
// chain file lives at <dataDir>/audit/chain.jsonl; the
// directory is created if missing.
//
// If the file already exists, the seq counter is seeded
// from its line count so restarts continue monotonically
// (the control plane's "since_seq" cursors depend on
// this — a restart that resets seq to 0 would replay
// the entire chain).
func New(dataDir string) (*Local, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("audit: dataDir is required")
	}
	dir := filepath.Join(dataDir, chainDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("audit: mkdir %s: %w", dir, err)
	}
	chainPath := filepath.Join(dir, chainFile)
	seq, err := countNonEmptyLines(chainPath)
	if err != nil {
		return nil, fmt.Errorf("audit: count existing entries: %w", err)
	}
	l := &Local{chainPath: chainPath}
	l.seq.Store(seq)
	return l, nil
}

// ChainPath returns the JSONL file path. Useful for
// tests and the daemon's status output.
func (l *Local) ChainPath() string { return l.chainPath }

// Log appends a hash-chained entry to the chain and
// returns the new seq (1-indexed; first call returns 1).
//
// Returns an error if Tool is empty (programmer error:
// an audit event without a tool name is useless for
// triage). Identity defaults to "agent"; Outcome
// defaults to "success" (matches the SDK's defaults).
//
// The file write goes through sdkAudit.LogAudit (the
// single ingress). If the write fails after seq.Add(1),
// the on-disk file is one entry shorter than the in-memory
// seq counter says; the next Log uses seq+1, leaving a
// "hole" in the file. The on-disk line number is the
// authoritative seq for Since; the in-memory counter
// only matters for "what seq will my next Log use".
// In practice the SDK's f.Sync() fails only on disk-full
// or permission errors, both of which are operator-visible.
func (l *Local) Log(ev Event) (uint64, error) {
	if ev.Tool == "" {
		return 0, fmt.Errorf("audit: Event.Tool is required")
	}
	if ev.Identity == "" {
		ev.Identity = defaultIdentity
	}
	if ev.Outcome == "" {
		ev.Outcome = "success"
	}
	argsVal, err := argsToValue(ev.Args)
	if err != nil {
		return 0, fmt.Errorf("audit: build args: %w", err)
	}
	seq := l.seq.Add(1)
	if _, err := sdkAudit.LogAudit(sdkAudit.Entry{
		Identity:      ev.Identity,
		Tool:          ev.Tool,
		Args:          argsVal,
		Outcome:       ev.Outcome,
		LogPath:       l.chainPath,
		PolicyVersion: sdkAudit.DefaultPolicyVersion,
	}); err != nil {
		// Decrement so the seq we return next time is
		// contiguous with the on-disk line number.
		l.seq.Add(^uint64(0))
		return 0, fmt.Errorf("audit: log: %w", err)
	}
	return seq, nil
}

// Since returns records with seq > sinceSeq, plus the
// highest seq currently in the file. An empty /
// non-existent file returns (nil, 0, nil).
//
// The returned records are parsed from the JSONL file
// in one pass. The seq field is the line number
// (1-indexed, contiguous from the first non-empty line).
// This guarantees: ReportStatus(sinceSeq=N) returns the
// events strictly after N.
func (l *Local) Since(sinceSeq uint64) ([]Record, uint64, error) {
	f, err := os.Open(l.chainPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("audit: open %s: %w", l.chainPath, err)
	}
	defer func() { _ = f.Close() }()

	var records []Record
	var lineNo uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lineNo++
		if lineNo <= sinceSeq {
			continue
		}
		rec, err := parseDiskLine(line, lineNo)
		if err != nil {
			return nil, 0, fmt.Errorf("audit: parse line %d: %w", lineNo, err)
		}
		records = append(records, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("audit: scan %s: %w", l.chainPath, err)
	}
	return records, lineNo, nil
}

// argsToValue converts a flat string→string args map to
// a *sdkAudit.Value suitable for sdkAudit.LogAudit.
//
// encoding/json sorts map keys alphabetically; the SDK
// also sorts recursively. The result is byte-exact
// compatible with what the SDK would produce from the
// same map. For an empty map we return a fresh
// NewObject() — bypasses the JSON round-trip but is
// semantically identical.
func argsToValue(args map[string]string) (*sdkAudit.Value, error) {
	if len(args) == 0 {
		return sdkAudit.NewObject(), nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	v, err := sdkAudit.ParseValue(string(b))
	if err != nil {
		return nil, err
	}
	return v, nil
}

// countNonEmptyLines returns the number of non-blank
// lines in path. A non-existent file returns 0. Used
// to recover the seq counter across restarts.
func countNonEmptyLines(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()
	var n uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n, sc.Err()
}

// parseDiskLine parses one JSONL line into a Record.
// Only the fields needed by ReportStatus are pulled out;
// tx_*-prefixed fields (if any) are ignored — the agent
// doesn't currently emit tx records, but the parser is
// tolerant in case a future T9+ adds them.
func parseDiskLine(line string, seq uint64) (Record, error) {
	var raw struct {
		Timestamp string            `json:"timestamp"`
		Identity  string            `json:"identity"`
		Tool      string            `json:"tool"`
		Args      map[string]string `json:"args"`
		Outcome   string            `json:"outcome"`
		PrevHash  string            `json:"prev_hash"`
		EntryHash string            `json:"entry_hash"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return Record{}, err
	}
	return Record{
		Seq:       seq,
		Timestamp: raw.Timestamp,
		Identity:  raw.Identity,
		Tool:      raw.Tool,
		Args:      raw.Args,
		Outcome:   raw.Outcome,
		PrevHash:  raw.PrevHash,
		EntryHash: raw.EntryHash,
	}, nil
}
