package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// tunnelChunkBytes bounds how much response body the node buffers per upstream
// TunnelResponse frame. Large responses (file downloads, notebook assets) are
// split across multiple frames so no single frame is unbounded.
const tunnelChunkBytes = 32 * 1024

// tunnelClient is the node-local HTTP client used to reach a session's services
// (jupyter, file server). It is separate from the server-connection client and
// never times out the body copy — the per-request context bounds it instead.
var tunnelClient = &http.Client{Timeout: 0}

// handleTunnel forwards a reverse-proxy request from the server to the session's
// local service and streams the response back up as TunnelResponse frames. It is
// the node end of the unified reverse-proxy gateway: the server holds the client
// connection and multiplexes HTTP over the NodeConnect stream; the node performs
// the actual local request. emit sends upstream frames on the live stream.
func (m *sessionManager) handleTunnel(ctx context.Context, req *agentcomposev2.NodeTunnelRequest, emit emitFunc) {
	tunnelID := req.GetTunnelId()
	target, err := m.resolveTunnelTarget(req.GetSessionId(), req.GetService(), req.GetPath())
	if err != nil {
		emitTunnelError(emit, tunnelID, http.StatusBadGateway, err.Error())
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, strings.ToUpper(strings.TrimSpace(req.GetMethod())), target, bytes.NewReader(req.GetBody()))
	if err != nil {
		emitTunnelError(emit, tunnelID, http.StatusBadGateway, fmt.Sprintf("build tunnel request: %v", err))
		return
	}
	for k, v := range req.GetHeaders() {
		httpReq.Header.Set(k, v)
	}

	resp, err := tunnelClient.Do(httpReq)
	if err != nil {
		// Classify dead-endpoint failures so the gateway can show a reason the
		// reader can act on instead of a raw dial error. The loopback file
		// service dies with the session's runtime process (or a node restart);
		// its recorded endpoint then lingers. We deliberately do NOT restart
		// the service here — surfacing the state is the contract; recovery is
		// the next message's dispatch.
		if isLoopbackConnectFailure(err) {
			emitTunnelError(emit, tunnelID, http.StatusServiceUnavailable,
				fmt.Sprintf("session service %q is not reachable (its runtime has exited): %v",
					req.GetService(), err))
			return
		}
		emitTunnelError(emit, tunnelID, http.StatusBadGateway, fmt.Sprintf("tunnel request failed: %v", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	headers := map[string]string{}
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}

	// First frame carries status + headers; body streams in subsequent frames.
	first := &agentcomposev2.NodeTunnelResponse{
		TunnelId: tunnelID,
		Status:   int32(resp.StatusCode),
		Headers:  headers,
	}
	buf := make([]byte, tunnelChunkBytes)
	n, readErr := resp.Body.Read(buf)
	if n > 0 {
		first.Body = append([]byte(nil), buf[:n]...)
	}
	if readErr == io.EOF {
		first.Done = true
	}
	if err := emit(tunnelFrame(first)); err != nil {
		return
	}
	if first.Done {
		return
	}

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			frame := &agentcomposev2.NodeTunnelResponse{
				TunnelId: tunnelID,
				Body:     append([]byte(nil), buf[:n]...),
			}
			if readErr == io.EOF {
				frame.Done = true
			}
			if err := emit(tunnelFrame(frame)); err != nil {
				return
			}
			if frame.Done {
				return
			}
			continue
		}
		if readErr != nil {
			done := &agentcomposev2.NodeTunnelResponse{TunnelId: tunnelID, Done: true}
			if readErr != io.EOF {
				done.Error = readErr.Error()
			}
			_ = emit(tunnelFrame(done))
			return
		}
	}
}

// isLoopbackConnectFailure identifies a dead session-local endpoint. Every
// resolveTunnelTarget result is a service registered by this process (normally
// 127.0.0.1:<ephemeral>), so a dial failure means the recorded listener is not
// serving — not that the public tunnel/control plane is down.
func isLoopbackConnectFailure(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && strings.EqualFold(opErr.Op, "dial") {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.ECONNRESET)
}

// resolveTunnelTarget maps a (session, service, path) to the local URL the node
// forwards to. Services are looked up on the session's recorded local endpoints
// (e.g. a jupyter port bound at start, or the node's built-in file service).
func (m *sessionManager) resolveTunnelTarget(sessionID, service, path string) (string, error) {
	m.mu.Lock()
	session, ok := m.sessions[strings.TrimSpace(sessionID)]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("session %s is not running on this node", sessionID)
	}
	base := session.serviceEndpoint(strings.TrimSpace(service))
	if base == "" {
		return "", fmt.Errorf("session %s has no %q service to proxy", sessionID, service)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return strings.TrimRight(base, "/") + path, nil
}

// handleProxy forwards a forward-proxy request from the server to an absolute
// URL and streams the response back up as NodeTunnelResponse frames. It is the
// node end of the node-mode HTTP proxy: the server wants a channel/API request
// to exit from the node's IP, so it sends a NodeProxyRequest describing the full
// HTTP request (method/url/headers/body); the node performs the outbound request
// itself (so TLS is terminated on the node, with the node's own HTTP client) and
// streams the response back. The node applies no business logic — it is a pure
// I/O relay. Runs in its own goroutine so a slow upstream does not block the
// dispatch loop.
func (m *sessionManager) handleProxy(ctx context.Context, req *agentcomposev2.NodeProxyRequest, emit emitFunc) {
	tunnelID := req.GetTunnelId()

	// 超时策略：普通请求 5 分钟封顶；但长连接流（SSE，如节点托管 MCP 的
	// mcp-proxy 端点）本就应该一直挂着，硬套 5 分钟会把它切断。所以先用可取消的
	// ctx + 定时器兜底，一旦确认响应是流式就停掉定时器，改由上层 ctx（会话/连接）
	// 决定生命周期。
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	deadline := time.AfterFunc(5*time.Minute, cancel)
	defer deadline.Stop()

	httpReq, err := http.NewRequestWithContext(reqCtx, strings.ToUpper(strings.TrimSpace(req.GetMethod())), req.GetUrl(), bytes.NewReader(req.GetBody()))
	if err != nil {
		emitTunnelError(emit, tunnelID, http.StatusBadGateway, fmt.Sprintf("build proxy request: %v", err))
		return
	}
	for k, v := range req.GetHeaders() {
		httpReq.Header.Set(k, v)
	}

	resp, err := tunnelClient.Do(httpReq)
	if err != nil {
		emitTunnelError(emit, tunnelID, http.StatusBadGateway, fmt.Sprintf("proxy request failed: %v", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if isStreamingResponse(resp) {
		deadline.Stop()
	}

	headers := map[string]string{}
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}

	// First frame carries status + headers; body streams in subsequent frames.
	first := &agentcomposev2.NodeTunnelResponse{
		TunnelId: tunnelID,
		Status:   int32(resp.StatusCode),
		Headers:  headers,
	}
	buf := make([]byte, tunnelChunkBytes)
	n, readErr := resp.Body.Read(buf)
	if n > 0 {
		first.Body = append([]byte(nil), buf[:n]...)
	}
	if readErr == io.EOF {
		first.Done = true
	}
	if err := emit(tunnelFrame(first)); err != nil {
		return
	}
	if first.Done {
		return
	}

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			frame := &agentcomposev2.NodeTunnelResponse{
				TunnelId: tunnelID,
				Body:     append([]byte(nil), buf[:n]...),
			}
			if readErr == io.EOF {
				frame.Done = true
			}
			if err := emit(tunnelFrame(frame)); err != nil {
				return
			}
			if frame.Done {
				return
			}
			continue
		}
		if readErr != nil {
			done := &agentcomposev2.NodeTunnelResponse{TunnelId: tunnelID, Done: true}
			if readErr != io.EOF {
				done.Error = readErr.Error()
			}
			_ = emit(tunnelFrame(done))
			return
		}
	}
}

// isStreamingResponse reports whether a proxied response is an open-ended stream
// rather than a bounded body. SSE (text/event-stream) is the case that matters:
// node-hosted stdio MCP is exposed through an mcp-proxy SSE endpoint, and such a
// stream legitimately stays open for the life of the MCP session. A chunked
// response with no content length is treated the same way.
func isStreamingResponse(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		return true
	}
	return resp.ContentLength < 0 && resp.Header.Get("Content-Length") == ""
}

// emitTunnelError sends a single terminal tunnel-response frame carrying an
// HTTP-style status and error message, ending the tunnel stream.
func emitTunnelError(emit emitFunc, tunnelID string, status int, message string) {
	_ = emit(tunnelFrame(&agentcomposev2.NodeTunnelResponse{
		TunnelId: tunnelID,
		Status:   int32(status),
		Done:     true,
		Error:    message,
	}))
}

func tunnelFrame(resp *agentcomposev2.NodeTunnelResponse) *agentcomposev2.NodeUpstreamFrame {
	return &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_TunnelResponse{TunnelResponse: resp},
	}
}
