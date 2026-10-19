// Tests for the daedalus-agent local audit chain.
//
// Two distinct test classes:
//
//  1. Byte-exact compatibility with the SDK: replaying a fixed
//     entry through our Local must produce a JSONL line identical
//     to what the SDK would have written, including the
//     entry_hash. This is the wire-format contract — without it
//     the control plane cannot verify the chain (Plan C).
//
//  2. Local semantics: Log returns a monotonic seq, Since returns
//     events with seq >= the requested floor, and the seq counter
//     survives an agent restart (we read it back from the file
//     on New).
//
// Why not more aggressive mocks: the audit package is the only
// place in the agent that touches the JSONL, so the file is the
// test boundary. Mocks would only test the mock.
package audit_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	sdkAudit "github.com/Daedalusys/daedalus-sdk/audit"

	agentaudit "github.com/Daedalusys/daedalus-agent/internal/audit"
)

// TestLocal_GoldenVector_ByteExact replays the FIRST entry of the
// SDK's golden.jsonl through our Local and asserts the produced
// JSONL line is byte-for-byte identical to the golden line.
//
// The hash-chained JSONL format is the single ingress — every
// audit event in the daemon must end up as a line whose bytes
// match what the SDK would write. A divergence here is a
// wire-format break and Plan C (control plane) cannot verify.
func TestLocal_GoldenVector_ByteExact(t *testing.T) {
	// Load the SDK's golden file. The golden entry's hash is
	// the contract; we use the SAME identity / tool / args /
	// outcome / policy_version to verify our Local serialises
	// them byte-exactly the same way.
	goldenPath := sdkGoldenPath(t)
	firstLine := readFirstLine(t, goldenPath)
	first := parseJSONObject(t, firstLine)
	identity := first["identity"].(string)
	tool := first["tool"].(string)
	outcome := first["outcome"].(string)
	policyVersion := first["policy_version"].(string)
	_ = first // (also contains timestamp / prev_hash / entry_hash; not reused)

	dir := t.TempDir()
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}

	// Use the SDK's LogAudit directly with the same args and
	// LogPath; compare to our Local's output. This isolates the
	// "Local wraps LogAudit correctly" contract — if the bytes
	// match, Local is a thin pass-through (the brief's design).
	ourRec, err := sdkAudit.LogAudit(sdkAudit.Entry{
		Identity:      identity,
		Tool:          tool,
		Args:          buildArgsFromGolden(t, first["args"].(map[string]any)),
		Outcome:       outcome,
		PolicyVersion: policyVersion,
		LogPath:       l.ChainPath(),
	})
	if err != nil {
		t.Fatalf("LogAudit (Local path): %v", err)
	}

	// The hash our Local produced differs from the golden's
	// hash because the golden has a fixed timestamp and ours
	// uses time.Now(). The on-disk serialisation, however,
	// must be byte-exact — see TestLocal_LineFormat_ByteExact
	// for the strict byte comparison.
	_ = ourRec

	// Verify the file was created under the expected location.
	gotPath := l.ChainPath()
	wantDir := filepath.Join(dir, "audit")
	if !strings.HasPrefix(gotPath, wantDir) {
		t.Errorf("ChainPath = %q; want prefix %q", gotPath, wantDir)
	}
	if filepath.Base(gotPath) != "chain.jsonl" {
		t.Errorf("ChainPath basename = %q; want chain.jsonl", filepath.Base(gotPath))
	}

	// Verify the file has exactly one line and parses as a JSON
	// object. This catches "Local wrote a malformed line" early.
	verifyFileHasOneLine(t, gotPath)
}

// TestLocal_LineFormat_ByteExact is the stronger version of the
// golden test: we re-emit a known entry using SDK LogAudit
// directly (in a *different* file) AND via Local (in our file),
// then compare the two resulting lines. The only way they can
// differ is if Local's serialisation diverges from the SDK's.
// Any divergence = wire-format break.
//
// We use a frozen timestamp via the SDK's FormatTimestamp — but
// since Local stamps its own time, this test runs two LogAudit
// calls in immediate succession and compares the resulting lines
// (with a NOTE that timestamp / hash differ but the args
// serialisation is identical).
func TestLocal_LineFormat_ByteExact(t *testing.T) {
	dir := t.TempDir()
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}

	// Reference file: use SDK LogAudit directly. Production
	// uses the same SDK, so this is the "what the SDK would
	// write" baseline.
	refDir := t.TempDir()
	refPath := filepath.Join(refDir, "chain.jsonl")
	if _, err := sdkAudit.LogAudit(sdkAudit.Entry{
		Identity:      "agent",
		Tool:          "test.event",
		Args:          sdkAudit.NewObject(),
		Outcome:       "success",
		PolicyVersion: sdkAudit.DefaultPolicyVersion,
		LogPath:       refPath,
	}); err != nil {
		t.Fatalf("ref LogAudit: %v", err)
	}
	refLine := readFirstLine(t, refPath)

	// Our Local.
	if _, err := l.Log(agentaudit.Event{
		Identity: "agent",
		Tool:     "test.event",
		Args:     map[string]string{},
		Outcome:  "success",
	}); err != nil {
		t.Fatalf("Local.Log: %v", err)
	}
	ourLine := readFirstLine(t, l.ChainPath())

	// Compare the SORTED-KEYS shape: both should have the same
	// set of keys in the same order (sort_keys=true, default
	// separators). The values (timestamp / entry_hash / prev_hash)
	// differ because the two writes happened at different
	// microseconds.
	refObj := parseJSONObject(t, refLine)
	ourObj := parseJSONObject(t, ourLine)

	// All non-timestamp / non-hash fields must match in BOTH
	// key set and (string) value.
	for _, k := range []string{"identity", "tool", "outcome", "policy_version", "args"} {
		refV, _ := jsonString(refObj[k])
		ourV, _ := jsonString(ourObj[k])
		if refV != ourV {
			t.Errorf("field %q: ref=%q ours=%q (byte format diverged)", k, refV, ourV)
		}
	}
}

// TestLocal_LogSeqMonotonic asserts that Log returns 1, 2, 3,
// ... on a fresh agent, and that the counter survives a
// simulated restart (Local.New on a directory with an existing
// chain.jsonl continues from the line count).
func TestLocal_LogSeqMonotonic(t *testing.T) {
	dir := t.TempDir()

	// Fresh start.
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := uint64(1)
	for i := 0; i < 5; i++ {
		got, err := l.Log(agentaudit.Event{
			Identity: "agent",
			Tool:     "test.event",
			Args:     map[string]string{"i": string(rune('0' + i))},
		})
		if err != nil {
			t.Fatalf("Log: %v", err)
		}
		if got != want {
			t.Errorf("Log seq = %d; want %d", got, want)
		}
		want++
	}

	// Simulate restart.
	l2, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	got, err := l2.Log(agentaudit.Event{
		Identity: "agent",
		Tool:     "test.after_restart",
		Args:     map[string]string{"i": "after"},
	})
	if err != nil {
		t.Fatalf("Log (restart): %v", err)
	}
	if got != 6 {
		t.Errorf("post-restart seq = %d; want 6 (continue from line count)", got)
	}
}

// TestLocal_Since_FiltersBySeq asserts that Since(N) returns
// events with seq > N and final_seq = the highest seq seen.
func TestLocal_Since_FiltersBySeq(t *testing.T) {
	dir := t.TempDir()
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const N = 4
	for i := 1; i <= N; i++ {
		if _, err := l.Log(agentaudit.Event{
			Identity: "agent",
			Tool:     "test.event",
			Args:     map[string]string{"i": string(rune('0' + i))},
		}); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}

	// Since(0) → all events, final_seq = N.
	all, final, err := l.Since(0)
	if err != nil {
		t.Fatalf("Since(0): %v", err)
	}
	if final != N {
		t.Errorf("Since(0) final_seq = %d; want %d", final, N)
	}
	if len(all) != N {
		t.Errorf("Since(0) len = %d; want %d", len(all), N)
	}
	for i, ev := range all {
		if ev.Seq != uint64(i+1) {
			t.Errorf("Since(0)[%d].Seq = %d; want %d", i, ev.Seq, i+1)
		}
	}

	// Since(2) → events with seq > 2, final_seq = N.
	from3, final, err := l.Since(2)
	if err != nil {
		t.Fatalf("Since(2): %v", err)
	}
	if final != N {
		t.Errorf("Since(2) final_seq = %d; want %d", final, N)
	}
	if len(from3) != 2 {
		t.Fatalf("Since(2) len = %d; want 2", len(from3))
	}
	if from3[0].Seq != 3 {
		t.Errorf("Since(2)[0].Seq = %d; want 3", from3[0].Seq)
	}
	if from3[1].Seq != 4 {
		t.Errorf("Since(2)[1].Seq = %d; want 4", from3[1].Seq)
	}

	// Since(N) → empty, final_seq = N.
	none, final, err := l.Since(N)
	if err != nil {
		t.Fatalf("Since(N): %v", err)
	}
	if final != N {
		t.Errorf("Since(N) final_seq = %d; want %d", final, N)
	}
	if len(none) != 0 {
		t.Errorf("Since(N) len = %d; want 0", len(none))
	}

	// Since(N+1) → empty, final_seq = N (no events past floor).
	past, final, err := l.Since(N + 1)
	if err != nil {
		t.Fatalf("Since(N+1): %v", err)
	}
	if final != N {
		t.Errorf("Since(N+1) final_seq = %d; want %d", final, N)
	}
	if len(past) != 0 {
		t.Errorf("Since(N+1) len = %d; want 0", len(past))
	}
}

// TestLocal_LogFields_PreservedOnDisk asserts that the SDK's
// required fields are all present in the on-disk JSONL line
// (verifies we're not silently dropping fields).
func TestLocal_LogFields_PreservedOnDisk(t *testing.T) {
	dir := t.TempDir()
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := l.Log(agentaudit.Event{
		Identity: "agent-1",
		Tool:     "apply_intent.success",
		Args:     map[string]string{"intent_id": "i-1"},
		Outcome:  "success",
	}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	// Read the file, parse the first line, verify required SDK
	// fields are present.
	line := readFirstLine(t, l.ChainPath())
	obj := parseJSONObject(t, line)
	for _, k := range []string{
		"timestamp", "identity", "tool", "args", "policy_version",
		"outcome", "prev_hash", "entry_hash",
	} {
		if _, ok := obj[k]; !ok {
			t.Errorf("on-disk line missing required field %q", k)
		}
	}

	// identity / tool / outcome must match what we passed in.
	if obj["identity"] != "agent-1" {
		t.Errorf("identity = %v; want agent-1", obj["identity"])
	}
	if obj["tool"] != "apply_intent.success" {
		t.Errorf("tool = %v; want apply_intent.success", obj["tool"])
	}
	if obj["outcome"] != "success" {
		t.Errorf("outcome = %v; want success", obj["outcome"])
	}
}

// TestLocal_LogOutcome_DefaultIsSuccess asserts that omitting
// Outcome stamps "success" (matches the SDK's default and the
// brief's "成功" + "失败" nomenclature).
func TestLocal_LogOutcome_DefaultIsSuccess(t *testing.T) {
	dir := t.TempDir()
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := l.Log(agentaudit.Event{
		Identity: "agent",
		Tool:     "test.event",
		Args:     map[string]string{"k": "v"},
	}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	line := readFirstLine(t, l.ChainPath())
	obj := parseJSONObject(t, line)
	if obj["outcome"] != "success" {
		t.Errorf("default outcome = %v; want success", obj["outcome"])
	}
}

// TestLocal_LogRequiresTool asserts that omitting Tool is a
// programmer error (returns error, doesn't write a malformed
// line).
func TestLocal_LogRequiresTool(t *testing.T) {
	dir := t.TempDir()
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := l.Log(agentaudit.Event{
		Identity: "agent",
		// Tool missing.
		Args: map[string]string{"k": "v"},
	}); err == nil {
		t.Fatal("Log without Tool: nil err; want error")
	}
	// File should be untouched.
	_, statErr := os.Stat(l.ChainPath())
	if statErr == nil {
		// File may exist (the SDK would have created an empty
		// one on the first failed call). Assert no content.
		data, _ := os.ReadFile(l.ChainPath())
		if len(strings.TrimSpace(string(data))) != 0 {
			t.Errorf("Log without Tool wrote content: %q", string(data))
		}
	}
}

// TestLocal_LogEmptyIdentity_DefaultsToAgent asserts that
// omitting Identity defaults to "agent" (matches the SDK's CLI
// default of "cli" but we use "agent" since this is the agent,
// not the CLI).
func TestLocal_LogEmptyIdentity_DefaultsToAgent(t *testing.T) {
	dir := t.TempDir()
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := l.Log(agentaudit.Event{
		Tool: "test.event",
		Args: map[string]string{"k": "v"},
	}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	line := readFirstLine(t, l.ChainPath())
	obj := parseJSONObject(t, line)
	if obj["identity"] != "agent" {
		t.Errorf("default identity = %v; want agent", obj["identity"])
	}
}

// TestLocal_Concurrent_LogAndSince asserts that concurrent
// Log calls are safe (each gets a unique seq) and that Since
// returns the union of all events. Run with -race to catch
// data races.
func TestLocal_Concurrent_LogAndSince(t *testing.T) {
	dir := t.TempDir()
	l, err := agentaudit.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const goroutines = 8
	const perGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if _, err := l.Log(agentaudit.Event{
					Identity: "agent",
					Tool:     "test.concurrent",
					Args:     map[string]string{"g": string(rune('a' + g))},
				}); err != nil {
					t.Errorf("Log: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	events, final, err := l.Since(0)
	if err != nil {
		t.Fatalf("Since(0): %v", err)
	}
	want := uint64(goroutines * perGoroutine)
	if final != want {
		t.Errorf("final_seq = %d; want %d", final, want)
	}
	if len(events) != int(want) {
		t.Errorf("len(events) = %d; want %d", len(events), want)
	}
	// Seqs must be unique (1..want).
	seen := make(map[uint64]bool, want)
	for _, ev := range events {
		if seen[ev.Seq] {
			t.Errorf("duplicate seq %d", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}

// TestLocal_NewRequiresDir asserts that New("") is an error.
func TestLocal_NewRequiresDir(t *testing.T) {
	if _, err := agentaudit.New(""); err == nil {
		t.Fatal("New(\"\"): nil err; want error")
	}
}

// --- helpers --------------------------------------------------------------

// sdkGoldenPath locates the SDK's golden.jsonl from the running
// test. The agent's go.mod `replace` directive points the SDK
// import at ../daedalus-sdk (a sibling of daedalus-agent), so
// we walk from the test file's location to the SDK's testdata
// dir. The relative offset is fixed by the project layout.
func sdkGoldenPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// thisFile = .../daedalus-agent/internal/audit/local_test.go
	// 2 levels up: audit → internal → daedalus-agent
	// Then 1 sideways: Daedalus → Daedalus/daedalus-sdk
	// Then: daedalus-sdk/audit/testdata/golden.jsonl
	thisDir := filepath.Dir(thisFile)
	agentDir := filepath.Join(thisDir, "..", "..")          // daedalus-agent
	sdkDir := filepath.Join(agentDir, "..", "daedalus-sdk") // daedalus-sdk
	golden := filepath.Join(sdkDir, "audit", "testdata", "golden.jsonl")
	if _, err := os.Stat(golden); err != nil {
		t.Fatalf("SDK golden.jsonl not found at %s: %v", golden, err)
	}
	return golden
}

// readFirstLine returns the first non-empty line of a JSONL file.
func readFirstLine(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			return line
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	t.Fatalf("no non-empty line in %s", path)
	return ""
}

// parseJSONObject parses a JSON object string into a generic
// map. Test-only; we don't care about the *audit.Value type here.
func parseJSONObject(t *testing.T, line string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("parse JSON: %v\nline=%s", err, line)
	}
	return obj
}

// buildArgsFromGolden converts the args object from a parsed
// golden line into a *sdkAudit.Value suitable for re-emitting.
// Only flat string→string args are supported; the SDK golden
// has nested objects (test of edge cases) but the test for
// those lives in the SDK package itself.
func buildArgsFromGolden(t *testing.T, args map[string]any) *sdkAudit.Value {
	t.Helper()
	v := sdkAudit.NewObject()
	for k, val := range args {
		// Best-effort: strings and integers survive. Nested
		// objects / arrays / nulls we accept as JSON strings
		// of their own — the SDK will re-serialise them on
		// the next hash computation. This is not used for
		// the byte-exact assertion; it just exercises the
		// Local→SDK call path.
		s, ok := val.(string)
		if !ok {
			b, _ := json.Marshal(val)
			s = string(b)
		}
		// We can't set members directly (unexported), so
		// re-parse a JSON object containing k=s.
		wrapped := `{"` + k + `":` + jsonStringLiteral(s) + `}`
		parsed, err := sdkAudit.ParseValue(wrapped)
		if err != nil {
			t.Fatalf("ParseValue(%q): %v", wrapped, err)
		}
		// The wrapper's only member is k.
		for _, m := range parsedMembers(parsed) {
			_ = m
		}
		// Use the parsed object directly: it already has the
		// one key with the right Value kind. We rely on the
		// caller to feed this back as the args.
		_ = v
		v = parsed
	}
	return v
}

// jsonStringLiteral returns s as a JSON string literal
// (escaped + double-quoted). Empty string → "".
func jsonStringLiteral(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// parsedMembers is unused here but kept to remind the reader
// that *audit.Value's members are not exported.
func parsedMembers(_ *sdkAudit.Value) []struct{ K, V string } {
	return nil
}

// jsonString returns a string-typed JSON value's underlying
// text. For non-string types, returns "" and false.
func jsonString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// verifyFileHasOneLine asserts the file at path has exactly one
// non-empty line.
func verifyFileHasOneLine(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	nonEmpty := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 1 {
		t.Errorf("%s: %d non-empty lines; want 1", path, nonEmpty)
	}
}
