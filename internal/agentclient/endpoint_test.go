package agentclient

import (
	"strings"
	"testing"
)

func TestEndpoint_NormalizesAndRejects(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string // 非空表示必须报错,且错误文案必须包含该子串
	}{
		{"https 带 scheme", "https://control.example:7443", "control.example:7443", ""},
		{"裸 host:port", "control.example:7443", "control.example:7443", ""},
		{"自定义 gRPC target 原样透传", "passthrough:///bufnet", "passthrough:///bufnet", ""},
		{"空串", "", "", "为空"},
		{"http 拒绝", "http://control.example:7443", "", "scheme"},
		{"https 缺 host", "https://", "", "为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Endpoint(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Endpoint(%q) 必须报错", tc.raw)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误文案 = %q, want 含 %q", err.Error(), tc.wantErr)
				}
				if got != "" {
					t.Fatalf("失败时返回值必须为空, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Endpoint(%q) 意外报错: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("Endpoint(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
