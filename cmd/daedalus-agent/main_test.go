package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDaemonParent_DefaultsToStart verifies that `daedalus-agent daemon`
// (no subcommand) dispatches to the same body as `daedalus-agent daemon
// start`, per the brief's "start (后台跑, 默认)" clause.
//
// The test uses a deliberately bad config path so loadConfig fails
// fast, BEFORE the daemon's run loop is entered. If the parent `daemon`
// correctly dispatches to start, the returned error will reference the
// bad config path; if dispatch were broken, cobra would print help and
// return nil (no error).
func TestDaemonParent_DefaultsToStart(t *testing.T) {
	const badPath = "/no/such/path/agent.yaml"

	cmd := newRootCmd()
	cmd.SetArgs([]string{"daemon", "--config", badPath})

	// Silence cobra's own stdout/stderr so the test output stays
	// clean; the error itself is what we assert on.
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected error from bad config path; got nil\n"+
			"stdout: %s\nstderr: %s",
			out.String(), errOut.String())
	}
	if !strings.Contains(err.Error(), badPath) {
		t.Errorf("error %q does not contain the bad config path %q; "+
			"this suggests the parent `daemon` did not dispatch to `start`",
			err.Error(), badPath)
	}
}

// TestDaemonStart_StillWorks pins the explicit `daemon start` path so
// the refactor (extracting the body into runDaemonStart and adding a
// RunE to the parent) cannot silently break the documented command.
func TestDaemonStart_StillWorks(t *testing.T) {
	const badPath = "/no/such/path/agent.yaml"

	cmd := newRootCmd()
	cmd.SetArgs([]string{"daemon", "start", "--config", badPath})

	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected error from bad config path; got nil\n"+
			"stdout: %s\nstderr: %s",
			out.String(), errOut.String())
	}
	if !strings.Contains(err.Error(), badPath) {
		t.Errorf("error %q does not contain the bad config path %q",
			err.Error(), badPath)
	}
}

// TestBareInvocation_PrintsHelp guards the judgment call documented in
// newDaemonCmd: bare `daedalus-agent` (no subcommand) does NOT
// auto-start the daemon — it prints help and exits 0. This prevents
// the production-surprise hazard of an agent that starts itself on
// any accidental invocation.
func TestBareInvocation_PrintsHelp(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{})

	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("bare invocation should not error; got %v\n"+
			"stdout: %s\nstderr: %s",
			err, out.String(), errOut.String())
	}
	// Cobra's help output mentions the program name. We don't pin
	// the exact text (cobra controls that), but we do require the
	// help to be written to stdout.
	if !strings.Contains(out.String(), "daedalus-agent") {
		t.Errorf("expected help text mentioning `daedalus-agent` on stdout; "+
			"got: %q", out.String())
	}
}
