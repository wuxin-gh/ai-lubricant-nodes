// Package agent is the shared node-connection layer for the execution and
// management node binaries. It owns everything that is identical across roles —
// dialing the server, the h2c/TLS transport choice, the reconnect loop, the
// NodeServerHello → NodeRegister handshake, the heartbeat, the stream send
// primitives, and command-ack framing — and delegates the role-specific
// downstream command dispatch to a DownstreamHandler.
//
// An execution node supplies a handler that runs sessions; a management node
// supplies one that launches execution nodes. Neither imports the other.
package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"ai-lubricant-nodes/common/auth/totp"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
	"ai-lubricant-nodes/common/proto/agentcompose/v2/agentcomposev2connect"
)

// Role names for the register frame. The two binaries are distinct, but the
// server still distinguishes them on the wire by the role in NodeRegister.
const (
	RoleExecution  = "execution"
	RoleManagement = "management"
	// RoleIosHost is the iOS device host: it dials in for identity/heartbeat/
	// version-report/self-upgrade only and drives its paired iPhones over a
	// separate device-control WebSocket. It runs no sessions and launches nothing.
	RoleIosHost = "ios_host"
)

// LookPath is indirected for testability; it also backs DockerAvailable and is
// reused by the role packages for their own capability probes.
var LookPath = exec.LookPath

// Options are the connection/registration parameters shared by both roles. The
// role packages hold their own extra options (work root, agent image) and build
// this from them.
type Options struct {
	Server    string
	NodeID    string
	Secret    string
	NodeName  string
	Role      string
	Version   string
	Providers []string
	Labels    map[string]string
	Docker    bool
	// SystemEnvAllowed advertises the system_env capability: the node accepts
	// env_mode=system sessions (editor runs against the operator's real HOME).
	// Set from --allow-system-env; off by default.
	SystemEnvAllowed bool
	TLSInsecure      bool
	MinBackoff       time.Duration
	MaxBackoff       time.Duration
	Heartbeat        time.Duration
}

// EmitFunc sends an upstream frame on whatever stream is currently live. It
// returns ErrStreamGone when the connection is down; callers keep the payload
// queued and retry after reconnect.
type EmitFunc func(*agentcomposev2.NodeUpstreamFrame) error

// ErrStreamGone is returned by EmitUpstream when no stream is currently live.
var ErrStreamGone = fmt.Errorf("node stream is closed")

// DownstreamHandler processes the downstream command frames for one node role.
// The common client handles NodeServerHello, the registration ack, heartbeats,
// and server Error frames itself; every other frame is passed to HandleFrame.
type DownstreamHandler interface {
	// HandleFrame processes one downstream command frame, using the client to
	// send acks/output back on the live stream.
	HandleFrame(ctx context.Context, c *Client, frame *agentcomposev2.NodeDownstreamFrame)
	// ActiveSessionIDs is reported in each heartbeat.
	ActiveSessionIDs() []string
	// ActiveToolRuns lets the server reconcile long-lived tunnel clients after a
	// control-stream reconnect.
	ActiveToolRuns() []*agentcomposev2.NodeActiveToolRun
	// StopAll is called once the connection drops so the handler can quiesce.
	StopAll()
}

type publicIPState struct {
	configRevision uint64
	ipv4URLs       []string
	ipv6URLs       []string
	ipv4           string
	ipv6           string
	ipv4ResolvedAt string
	ipv6ResolvedAt string
	ipv4Disabled   bool
	ipv6Disabled   bool
}

// proxyState is the persisted node-egress-proxy snapshot pushed by the server.
// Empty proxyMode means direct download. self-upgrade/runtime-upgrade use the
// per-frame proxy when the frame carries one (admin override for that single
// upgrade); otherwise they fall back to this persisted snapshot.
type proxyState struct {
	revision       uint64
	proxyMode      string
	proxyURL       string
	proxyURLPrefix string
}

// Client owns one node's connection lifetime across reconnects: the Connect
// client, the live stream, and the node identity assigned at registration.
type Client struct {
	opts    Options
	logger  *slog.Logger
	client  agentcomposev2connect.NodeServiceClient
	handler DownstreamHandler

	mu            sync.Mutex
	nodeID        string
	currentStream *connect.BidiStreamForClient[agentcomposev2.NodeUpstreamFrame, agentcomposev2.NodeDownstreamFrame]
	publicIP      publicIPState
	publicIPWake  chan struct{}
	proxy         proxyState
}

// NewClient builds a client for the given options. Call SetHandler before Run.
func NewClient(opts Options, logger *slog.Logger) *Client {
	httpClient := newNodeHTTPClient(opts)
	return &Client{
		opts:         opts,
		logger:       logger,
		client:       agentcomposev2connect.NewNodeServiceClient(httpClient, opts.Server),
		publicIPWake: make(chan struct{}, 1),
	}
}

// SetHandler installs the role-specific downstream handler. It is separate from
// NewClient so the handler can be constructed with the client's stable
// EmitUpstream closure.
func (c *Client) SetHandler(h DownstreamHandler) { c.handler = h }

// Logger exposes the client logger to handlers.
func (c *Client) Logger() *slog.Logger { return c.logger }

// newNodeHTTPClient builds an HTTP client for Connect. The NodeConnect RPC is a
// long-lived bidi stream, so the client must never time out the request; per-op
// deadlines are enforced at the session level instead. h2c (cleartext HTTP/2)
// is used for http:// servers and local sockets; https:// uses real TLS.
func newNodeHTTPClient(opts Options) *http.Client {
	isHTTPS := strings.HasPrefix(strings.ToLower(opts.Server), "https://")
	if isHTTPS {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if opts.TLSInsecure {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in dev flag
		}
		return &http.Client{Transport: transport}
	}
	// h2c: allow HTTP/2 over cleartext so Connect bidi streams work without TLS.
	h2c := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: h2c}
}

// Run drives the reconnect loop: dial, register, serve the stream until it
// breaks, then back off and retry until the context is canceled.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.opts.MinBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		startedAt := time.Now()
		c.logger.Info("node connection attempt", "server", c.opts.Server)
		serveErr := c.serve(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A connection that stayed up for a while resets the backoff so a
		// long-lived session that eventually drops reconnects promptly.
		if time.Since(startedAt) > c.opts.MaxBackoff {
			backoff = c.opts.MinBackoff
		}
		c.logger.Warn("node connection lost; reconnecting", "error", serveErr, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > c.opts.MaxBackoff {
			backoff = c.opts.MaxBackoff
		}
	}
}

// serve opens the NodeConnect stream, registers, then runs the heartbeat and
// downstream dispatch loops until the stream breaks or ctx is canceled.
func (c *Client) serve(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.logger.Info("opening NodeConnect stream")
	stream := c.client.NodeConnect(streamCtx)
	defer func() { _ = stream.CloseRequest() }()

	c.setCurrentStream(stream)
	defer c.setCurrentStream(nil)

	// CallBidiStream (unlike CallBidiStreamSimple) does NOT eagerly send the
	// request headers — they're only emitted on the first Send. The server's
	// NodeConnect sends ServerHello the moment it sees the request, so without
	// an explicit header-send here the server never learns the stream exists
	// and the Receive below blocks forever on responseReady. Send(nil) ships
	// the headers with a zero-length body (connect-go's nopPayload), which is
	// exactly the header-only kick CallBidiStreamSimple does internally.
	if err := stream.Send(nil); err != nil {
		return fmt.Errorf("send nodeconnect request headers: %w", err)
	}

	// The server sends its authoritative time first (NodeServerHello). We derive
	// the TOTP code from that time rather than our own clock, so both sides
	// validate against the same reference instant.
	c.logger.Info("waiting for server hello")
	hello, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("await server hello: %w", err)
	}
	serverHello := hello.GetServerHello()
	if serverHello == nil {
		return fmt.Errorf("expected server hello, got %T", hello.GetFrame())
	}
	serverTime, err := time.Parse(time.RFC3339Nano, serverHello.GetServerTime())
	if err != nil {
		return fmt.Errorf("parse server time %q: %w", serverHello.GetServerTime(), err)
	}
	c.logger.Info("server hello received")

	if err := c.register(ctx, stream, serverTime); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	c.logger.Info("registration frame sent", "node_id", c.opts.NodeID)
	// Next downstream frame must be the registration ack carrying node_id.
	c.logger.Info("waiting for registration ack")
	first, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("await registration ack: %w", err)
	}
	registered := first.GetRegistered()
	if registered == nil {
		return fmt.Errorf("expected registration ack, got %T", first.GetFrame())
	}
	c.setNodeID(registered.GetNodeId())
	c.logger.Info("node registered",
		"node_id", registered.GetNodeId(),
		"status", registered.GetStatus().String(),
		"online", registered.GetOnline())
	if registered.GetStatus() == agentcomposev2.NodeStatus_NODE_STATUS_REVOKED {
		return fmt.Errorf("node is revoked by the server")
	}

	// Heartbeat and public-IP resolver loops run alongside downstream dispatch.
	// The resolver never blocks registration, command handling, or heartbeats.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// A failed heartbeat send means the stream is gone. The heartbeat loop
		// cancels streamCtx so the blocked dispatchLoop Receive returns and
		// serve() unwinds into Run()'s reconnect — otherwise the node would
		// stay silently offline until the process restarts.
		c.heartbeatLoop(streamCtx, cancel, stream)
	}()
	go func() {
		defer wg.Done()
		c.publicIPLoop(streamCtx)
	}()

	dispatchErr := c.dispatchLoop(streamCtx, stream)
	cancel()
	wg.Wait()
	c.handler.StopAll()
	return dispatchErr
}

func (c *Client) register(ctx context.Context, stream *connect.BidiStreamForClient[agentcomposev2.NodeUpstreamFrame, agentcomposev2.NodeDownstreamFrame], serverTime time.Time) error {
	secret, err := totp.DecodeSecret(c.opts.Secret)
	if err != nil {
		return fmt.Errorf("decode node secret: %w", err)
	}
	code := totp.Generate(secret, serverTime)
	return stream.Send(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_Register{
			Register: &agentcomposev2.NodeRegister{
				NodeId:   c.opts.NodeID,
				TotpCode: code,
				NodeName: c.opts.NodeName,
				Role:     nodeRoleToProto(c.opts.Role),
				Capabilities: &agentcomposev2.NodeCapabilities{
					Os:        runtime.GOOS,
					Arch:      runtime.GOARCH,
					Docker:    DockerAvailable(),
					Providers: c.opts.Providers,
					Editors:   EditorCapabilities(ctx),
					Labels:    c.capabilityLabels(ctx),
				},
			},
		},
	})
}

// capabilityLabels merges operator labels with local, non-blocking host probes.
// Public IP discovery runs only in the background after the server supplies its
// global lookup configuration; registration never waits on external websites.
func (c *Client) capabilityLabels(ctx context.Context) map[string]string {
	labels := SystemLabels()
	for k, v := range internalNetworkLabels() {
		labels[k] = v
	}
	for k, v := range EditorLabels(ctx) {
		labels[k] = v
	}
	for k, v := range HostToolLabels(ctx) {
		labels[k] = v
	}
	if v := strings.TrimSpace(c.opts.Version); v != "" {
		labels["client_version"] = v
	}
	for k, v := range c.opts.Labels {
		labels[k] = v
	}
	// Terminal frames are part of the node protocol. This is a binary capability,
	// not an operator label, so it must not be overridden by user configuration.
	labels["terminal"] = "true"
	// Structured host-exec frame: server pushes a NodeHostExecRequest and the
	// node returns separated stdout/stderr/exit. Same override rule as terminal.
	labels["host_exec"] = "true"
	// Chunked binary file uploads use dedicated protocol frames so they remain
	// binary-safe and do not inherit host shell command-line size limits.
	labels["file_upload"] = "true"
	// Long-running external tool runs (tunnel manager: frpc/cloudflared/npc).
	// The node streams stdout/stderr + exit as NodeToolRunEvent frames by run_id.
	labels["tool_run"] = "true"
	labels["tool_run_runtime_v2"] = "true"
	// system_env advertises that this node accepts env_mode=system sessions
	// (editor runs against the operator's real HOME). Off unless the operator
	// started the node with --allow-system-env; the server surfaces it so the
	// task form can enable the system tier only where the node allows it.
	if c.opts.SystemEnvAllowed {
		labels["system_env"] = "true"
	}
	return labels
}

func (c *Client) heartbeatLoop(ctx context.Context, cancel context.CancelFunc, stream *connect.BidiStreamForClient[agentcomposev2.NodeUpstreamFrame, agentcomposev2.NodeDownstreamFrame]) {
	ticker := time.NewTicker(c.opts.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			heartbeat := &agentcomposev2.NodeHeartbeat{
				NodeId:           c.nodeIDValue(),
				ActiveSessionIds: c.handler.ActiveSessionIDs(),
				ActiveToolRuns:   c.handler.ActiveToolRuns(),
			}
			if report := c.publicIPReport(); report != nil {
				heartbeat.PublicIpReport = report
			}
			// Host terminals survive a dropped NodeConnect. Advertising their IDs
			// lets the server reconcile and issue NodeTerminalAttach after a
			// reconnect instead of assuming every PTY died with the stream.
			if terminalProvider, ok := c.handler.(interface{ ActiveTerminalIDs() []string }); ok {
				heartbeat.ActiveTerminalIds = terminalProvider.ActiveTerminalIDs()
			}
			frame := &agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_Heartbeat{Heartbeat: heartbeat},
			}
			if err := stream.Send(frame); err != nil {
				c.logger.Warn("heartbeat send failed", "error", err)
				// The stream is dead. Cancel it so dispatchLoop's blocked
				// Receive returns and serve() falls through to reconnect;
				// returning alone would leave the receiver hung until the
				// process dies, and the node would never come back online.
				cancel()
				return
			}
		}
	}
}

// dispatchLoop reads downstream commands and routes them to the handler. It
// returns when the stream ends or errors. Server Error frames are handled here
// (role-independent); everything else goes to the handler.
func (c *Client) dispatchLoop(ctx context.Context, stream *connect.BidiStreamForClient[agentcomposev2.NodeUpstreamFrame, agentcomposev2.NodeDownstreamFrame]) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame, err := stream.Receive()
		if err != nil {
			return err
		}
		if config := frame.GetPublicIpLookupConfig(); config != nil {
			c.applyPublicIPConfig(config)
			continue
		}
		if proxyCfg := frame.GetNodeProxyConfig(); proxyCfg != nil {
			c.applyNodeProxyConfig(proxyCfg)
			continue
		}
		if errFrame := frame.GetError(); errFrame != nil {
			c.logger.Warn("server error frame", "code", errFrame.GetCode(), "message", errFrame.GetMessage())
			if errFrame.GetTerminal() {
				return fmt.Errorf("server terminal error: %s", errFrame.GetMessage())
			}
			continue
		}
		c.handler.HandleFrame(ctx, c, frame)
	}
}

// applyNodeProxyConfig stores the server-pushed egress-proxy snapshot,
// revision-gated so an older replay (reconnect after a newer update) is ignored.
func (c *Client) applyNodeProxyConfig(config *agentcomposev2.NodeProxyConfig) {
	if config == nil {
		return
	}
	c.mu.Lock()
	if config.GetRevision() <= c.proxy.revision && c.proxy.revision != 0 {
		c.mu.Unlock()
		return
	}
	c.proxy = proxyState{
		revision:       config.GetRevision(),
		proxyMode:      config.GetProxyMode(),
		proxyURL:       config.GetProxyUrl(),
		proxyURLPrefix: config.GetProxyUrlPrefix(),
	}
	c.mu.Unlock()
	c.logger.Info("node egress proxy applied", "revision", config.GetRevision(), "mode", config.GetProxyMode())
}

// downloadProxy returns the persisted egress-proxy snapshot as a proxySpec.
// self-upgrade / runtime-upgrade use the per-frame proxy when the frame carries
// one (admin override for that single upgrade); otherwise they fall back here.
func (c *Client) DownloadProxy() proxySpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return proxySpec{
		mode:      c.proxy.proxyMode,
		url:       c.proxy.proxyURL,
		urlPrefix: c.proxy.proxyURLPrefix,
	}
}

func (c *Client) applyPublicIPConfig(config *agentcomposev2.NodePublicIPLookupConfig) {

	if config == nil {
		return
	}
	c.mu.Lock()
	if config.GetRevision() <= c.publicIP.configRevision {
		c.mu.Unlock()
		return
	}
	c.publicIP.configRevision = config.GetRevision()
	c.publicIP.ipv4URLs = append([]string(nil), config.GetIpv4Urls()...)
	c.publicIP.ipv6URLs = append([]string(nil), config.GetIpv6Urls()...)
	c.publicIP.ipv4Disabled = len(c.publicIP.ipv4URLs) == 0
	c.publicIP.ipv6Disabled = len(c.publicIP.ipv6URLs) == 0
	if c.publicIP.ipv4Disabled {
		c.publicIP.ipv4 = ""
		c.publicIP.ipv4ResolvedAt = ""
	}
	if c.publicIP.ipv6Disabled {
		c.publicIP.ipv6 = ""
		c.publicIP.ipv6ResolvedAt = ""
	}
	c.mu.Unlock()
	c.logger.Info("public IP lookup configuration applied", "revision", config.GetRevision(), "ipv4_sources", len(config.GetIpv4Urls()), "ipv6_sources", len(config.GetIpv6Urls()))
	select {
	case c.publicIPWake <- struct{}{}:
	default:
	}
}

func (c *Client) publicIPLoop(ctx context.Context) {
	ticker := time.NewTicker(publicIPRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.publicIPWake:
			c.refreshPublicIPs(ctx)
		case <-ticker.C:
			c.refreshPublicIPs(ctx)
		}
	}
}

func (c *Client) refreshPublicIPs(ctx context.Context) {
	c.mu.Lock()
	revision := c.publicIP.configRevision
	ipv4URLs := append([]string(nil), c.publicIP.ipv4URLs...)
	ipv6URLs := append([]string(nil), c.publicIP.ipv6URLs...)
	c.mu.Unlock()
	if revision == 0 {
		return
	}

	// IPv4 与 IPv6 两个地址族同时解析；每个族内部仍按配置顺序串行请求。
	// 一族失败不影响另一族：各自独立计票，先完成的先更新共享状态。
	var wg sync.WaitGroup
	resolveFamily := func(urls []string, family int, label string) {
		defer wg.Done()
		ip, err := resolvePublicIP(ctx, urls, family)
		if err != nil {
			c.logger.Warn("public IP verification failed", "family", label, "revision", revision, "error", err)
			return
		}
		c.updatePublicIPResult(revision, family, ip)
	}
	if len(ipv4URLs) > 0 {
		wg.Add(1)
		go resolveFamily(ipv4URLs, 4, "IPv4")
	}
	if len(ipv6URLs) > 0 {
		wg.Add(1)
		go resolveFamily(ipv6URLs, 6, "IPv6")
	}
	wg.Wait()
}

func (c *Client) updatePublicIPResult(revision uint64, family int, ip string) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	if revision != c.publicIP.configRevision {
		c.mu.Unlock()
		return
	}
	changed := false
	if family == 4 {
		changed = c.publicIP.ipv4 != ip
		c.publicIP.ipv4 = ip
		c.publicIP.ipv4ResolvedAt = now
		c.publicIP.ipv4Disabled = false
	} else {
		changed = c.publicIP.ipv6 != ip
		c.publicIP.ipv6 = ip
		c.publicIP.ipv6ResolvedAt = now
		c.publicIP.ipv6Disabled = false
	}
	c.mu.Unlock()
	if changed {
		c.logger.Info("public IP changed", "family", family, "address", ip, "revision", revision)
	}
}

func (c *Client) publicIPReport() *agentcomposev2.NodePublicIPReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.publicIP
	if state.configRevision == 0 {
		return nil
	}
	return &agentcomposev2.NodePublicIPReport{
		ConfigRevision: state.configRevision,
		Ipv4:           state.ipv4,
		Ipv6:           state.ipv6,
		Ipv4ResolvedAt: state.ipv4ResolvedAt,
		Ipv6ResolvedAt: state.ipv6ResolvedAt,
		Ipv4Disabled:   state.ipv4Disabled,
		Ipv6Disabled:   state.ipv6Disabled,
	}
}

// EmitUpstream sends on whatever stream is currently live. Handler goroutines
// (session output pumps, tunnels) outlive individual dispatch calls, so they
// reach the stream through this accessor; if the connection has dropped, output
// stays queued and this returns ErrStreamGone.
func (c *Client) EmitUpstream(frame *agentcomposev2.NodeUpstreamFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentStream == nil {
		return ErrStreamGone
	}
	return c.currentStream.Send(frame)
}

// SendAck acks a command with the optional session list (for ListSessions).
func (c *Client) SendAck(frameID string, cmdErr error, sessions []*agentcomposev2.NodeSessionSummary) {
	ack := &agentcomposev2.NodeCommandAck{
		ServerFrameId: frameID,
		Ok:            cmdErr == nil,
		Sessions:      sessions,
	}
	if cmdErr != nil {
		ack.Error = cmdErr.Error()
	}
	frame := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_CommandAck{CommandAck: ack},
	}
	if err := c.EmitUpstream(frame); err != nil {
		c.logger.Warn("command ack send failed", "error", err)
	}
}

// SendConfigAck acks a config command (Configure*/Apply*/Mode) with the revision
// the node persisted and whether the running editor must be restarted to load it.
func (c *Client) SendConfigAck(frameID string, cmdErr error, appliedRevision, effectiveRevision uint64, restartRequired bool) {
	ack := &agentcomposev2.NodeCommandAck{
		ServerFrameId:     frameID,
		Ok:                cmdErr == nil,
		AppliedRevision:   appliedRevision,
		EffectiveRevision: effectiveRevision,
		RestartRequired:   restartRequired,
	}
	if cmdErr != nil {
		ack.Error = cmdErr.Error()
	}
	frame := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_CommandAck{CommandAck: ack},
	}
	if err := c.EmitUpstream(frame); err != nil {
		c.logger.Warn("config ack send failed", "error", err)
	}
}

// SendEditorAck acks a ManageEditor command (install/upgrade). On success
// editorVersion carries the freshly probed version so the server can refresh
// the node's advertised capabilities without waiting for the next heartbeat.
func (c *Client) SendEditorAck(frameID string, cmdErr error, editorVersion string) {
	ack := &agentcomposev2.NodeCommandAck{
		ServerFrameId: frameID,
		Ok:            cmdErr == nil,
		EditorVersion: editorVersion,
	}
	if cmdErr != nil {
		ack.Error = cmdErr.Error()
	}
	frame := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_CommandAck{CommandAck: ack},
	}
	if err := c.EmitUpstream(frame); err != nil {
		c.logger.Warn("editor ack send failed", "error", err)
	}
}

// SendEnvironmentInventoryAck acks an InspectEnvironment command, carrying the
// list of resources the node observed physically installed in the environment
// HOME. On error the ack is not-ok with the reason; the inventory is nil.
func (c *Client) SendEnvironmentInventoryAck(frameID string, cmdErr error, inventory []*agentcomposev2.NodeEnvironmentEntry) {
	ack := &agentcomposev2.NodeCommandAck{
		ServerFrameId: frameID,
		Ok:            cmdErr == nil,
	}
	if cmdErr != nil {
		ack.Error = cmdErr.Error()
	} else {
		ack.EnvironmentInventory = inventory
	}
	frame := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_CommandAck{CommandAck: ack},
	}
	if err := c.EmitUpstream(frame); err != nil {
		c.logger.Warn("environment inventory ack send failed", "error", err)
	}
}

// SendSystemEnvInventoryAck acks an InspectSystemEnv / SyncSystemEnv command with
// what the node observed in (or just changed about) the operator's real HOME.
// Inspect fills it with the whole inventory; sync fills it with the entries it
// installed/skipped/removed, so the caller can report per-entry outcomes without a
// second round trip. On error the ack is not-ok and the inventory is nil.
func (c *Client) SendSystemEnvInventoryAck(frameID string, cmdErr error, inventory []*agentcomposev2.NodeSystemEnvEntry) {
	ack := &agentcomposev2.NodeCommandAck{
		ServerFrameId: frameID,
		Ok:            cmdErr == nil,
	}
	if cmdErr != nil {
		ack.Error = cmdErr.Error()
	} else {
		ack.SystemEnvInventory = inventory
	}
	frame := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_CommandAck{CommandAck: ack},
	}
	if err := c.EmitUpstream(frame); err != nil {
		c.logger.Warn("system env inventory ack send failed", "error", err)
	}
}

func (c *Client) setCurrentStream(stream *connect.BidiStreamForClient[agentcomposev2.NodeUpstreamFrame, agentcomposev2.NodeDownstreamFrame]) {
	c.mu.Lock()
	c.currentStream = stream
	c.mu.Unlock()
}

func (c *Client) setNodeID(id string) {
	c.mu.Lock()
	c.nodeID = id
	c.mu.Unlock()
}

func (c *Client) nodeIDValue() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nodeID
}

// nodeRoleToProto maps the role string onto the wire enum.
func nodeRoleToProto(role string) agentcomposev2.NodeRole {
	switch role {
	case RoleManagement:
		return agentcomposev2.NodeRole_NODE_ROLE_MANAGEMENT
	case RoleIosHost:
		return agentcomposev2.NodeRole_NODE_ROLE_IOS_HOST
	default:
		return agentcomposev2.NodeRole_NODE_ROLE_EXECUTION
	}
}

// DockerAvailable reports whether the docker CLI is on PATH.
func DockerAvailable() bool {
	_, err := LookPath("docker")
	return err == nil
}
