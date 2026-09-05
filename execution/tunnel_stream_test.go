package main

import (
	"net/http"
	"testing"
)

// 形态 C（节点托管 stdio -> 代理成 remote）靠 SSE 长连接穿隧道。handleProxy 原本对
// 每个代理请求硬套 5 分钟 context，会把合法的长连接切断；isStreamingResponse 是那条
// 「撤掉硬超时」判断的依据，所以它的分类必须被锁住。
func TestIsStreamingResponse(t *testing.T) {
	cases := []struct {
		name string
		resp *http.Response
		want bool
	}{
		{
			name: "SSE 是流式",
			resp: &http.Response{
				Header:        http.Header{"Content-Type": {"text/event-stream"}},
				ContentLength: -1,
			},
			want: true,
		},
		{
			name: "SSE 带 charset 也算",
			resp: &http.Response{
				Header:        http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}},
				ContentLength: -1,
			},
			want: true,
		},
		{
			name: "有 Content-Length 的 JSON 不是流式",
			resp: &http.Response{
				Header:        http.Header{"Content-Type": {"application/json"}, "Content-Length": {"123"}},
				ContentLength: 123,
			},
			want: false,
		},
		{
			name: "无长度的 chunked 按流式处理",
			resp: &http.Response{
				Header:        http.Header{"Content-Type": {"application/octet-stream"}},
				ContentLength: -1,
			},
			want: true,
		},
		{
			name: "空 body 且已知长度不是流式",
			resp: &http.Response{
				Header:        http.Header{"Content-Type": {"text/plain"}, "Content-Length": {"0"}},
				ContentLength: 0,
			},
			want: false,
		},
	}
	for _, tc := range cases {
		if got := isStreamingResponse(tc.resp); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
