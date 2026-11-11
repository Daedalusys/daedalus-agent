package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

func TestReportCmd_BuildsRequestAndPrintsEvents(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	var captured *agentv1.ReportStatusRequest
	installStubDial(t, &fakeAgentClient{reportFn: func(_ context.Context, in *agentv1.ReportStatusRequest, _ ...grpc.CallOption) (*agentv1.ReportStatusResponse, error) {
		captured = in
		return &agentv1.ReportStatusResponse{
			Events: []*agentv1.AuditEvent{
				{Seq: 6, EventType: "apply_intent.success", CurrentHash: "aa"},
				{Seq: 7, EventType: "rollback.start", CurrentHash: "bb"},
			},
			FinalSeq: 7,
		}, nil
	}})

	cmd := newReportCmd(&cfgPath)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--since", "5", "--max", "100"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("report 失败: %v", err)
	}
	if captured.GetSinceAuditSeq() != 5 || captured.GetMaxEvents() != 100 {
		t.Fatalf("请求 = %+v, want since=5 max=100", captured)
	}
	// protojson 把 64 位整数编码成 JSON 字符串(避免 JS 精度丢失),所以
	// 断言里带引号;这是 wire 层面的既定形态,不是本命令的格式选择。
	for _, want := range []string{`"final_seq": "7"`, `"seq": "6"`, `"event_type": "apply_intent.success"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout 缺 %q:\n%s", want, out.String())
		}
	}
}

func TestReportCmd_DefaultsToZero(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	var captured *agentv1.ReportStatusRequest
	installStubDial(t, &fakeAgentClient{reportFn: func(_ context.Context, in *agentv1.ReportStatusRequest, _ ...grpc.CallOption) (*agentv1.ReportStatusResponse, error) {
		captured = in
		return &agentv1.ReportStatusResponse{}, nil
	}})

	cmd := newReportCmd(&cfgPath)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("report 失败: %v", err)
	}
	if captured.GetSinceAuditSeq() != 0 || captured.GetMaxEvents() != 0 {
		t.Fatalf("默认值必须为 0/0(= 全部、不限条数), got %+v", captured)
	}
}

func TestReportCmd_TransportErrorIsNonZero(t *testing.T) {
	cfgPath := writeTestConfig(t, t.TempDir())
	installStubDial(t, &fakeAgentClient{reportFn: func(context.Context, *agentv1.ReportStatusRequest, ...grpc.CallOption) (*agentv1.ReportStatusResponse, error) {
		return nil, status.Error(codes.DeadlineExceeded, "timeout")
	}})

	cmd := newReportCmd(&cfgPath)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("传输错误必须非零退出")
	}
}
