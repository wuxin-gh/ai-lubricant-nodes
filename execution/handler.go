// Package execution is the execution node binary's core: it runs provider
// sessions on the local host (or in docker) on behalf of the server. It plugs
// into the shared connection layer (ai-lubricant-nodes/common/agent) by
// implementing agent.DownstreamHandler — the shared client owns the connection,
// registration, heartbeat, and framing; this handler owns the session commands.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ai-lubricant-nodes/common/agent"
	"ai-lubricant-nodes/common/build"
	"ai-lubricant-nodes/common/ioshost"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// sessionOptions are the execution-node knobs the session manager reads: where
// session working trees live, which providers this node advertises, and whether
// the docker driver is offered.
type sessionOptions struct {
	workRoot  string
	providers []string
	docker    bool
	// systemEnvAllowed opts this node into env_mode="system", where a session's
	// editor runs against the node operator's real HOME (installed toolchain and
	// CLI login state) instead of an isolated one. That hands the whole home —
	// ssh keys, provider credentials — to the agent, so it stays off unless the
	// operator asks for it and the node refuses the mode otherwise.
	systemEnvAllowed bool
}

// Handler is the execution node's downstream command handler. It owns the
// session manager and routes each session command onto it, acking back through
// the shared client's stream. It also serves build frames so a macOS execution
// host can build WDA (or future user-app recipes) from the project page, and —
// on a host where usbmuxd is reachable (--ios auto) — the iOS device management
// frames, so a Mac/Windows box with an iPhone attached can be scanned and driven
// without installing a separate node-ios binary.
type Handler struct {
	sessions  *sessionManager
	terminals *agent.TerminalManager
	toolruns  *agent.ToolRunManager
	builds    *build.Runner
	xcode     *agent.XcodeJobRunner
	// ios serves the iOS device management frames. Nil when this host does not
	// offer the capability (no usbmuxd, or --ios off), in which case those
	// frames fall through to the default branch below.
	ios *ioshost.FrameHandler
}

// NewHandler builds an execution handler bound to the given client. The session
// manager's upstream callbacks send on the client's live stream, so output and
// results survive reconnects (the client re-points the stream underneath).
//
// iosMgmtAllowed mirrors the advertised ios_mgmt capability: when false, no
// device manager is constructed at all and the iOS frames are rejected by the
// default branch (the server gates on the label, so this is defensive).
func NewHandler(ctx context.Context, c *agent.Client, nodeID, workRoot string, providers []string, docker bool, systemEnvAllowed, iosMgmtAllowed bool) *Handler {
	opts := sessionOptions{workRoot: workRoot, providers: providers, docker: docker, systemEnvAllowed: systemEnvAllowed}
	sessions := newSessionManager(opts, c.Logger(), c.EmitUpstream, c.EmitUpstream, c.EmitUpstream, c.EmitUpstream)
	// MCP wire specs now carry a relative path; the node resolves it against the
	// gateway origin the server announced in the hello frame (same host the node
	// connected to, so the server needs no configured public gateway URL).
	sessions.gatewayOrigin = c.GatewayOrigin
	terminals := agent.NewTerminalManager(c.EmitUpstream, c.Logger())
	terminals.StartReaper()
	toolruns := agent.NewToolRunManager(c.EmitUpstream, c.Logger())
	builds := build.NewRunner(c.EmitUpstream, c.Logger(), c)
	return &Handler{
		sessions:  sessions,
		terminals: terminals,
		toolruns:  toolruns,
		builds:    builds,
		xcode:     agent.NewXcodeJobRunner(c.EmitUpstream, c.Logger(), c.DownloadProxy),
		ios:       newIosFrameHandler(ctx, c, nodeID, iosMgmtAllowed),
	}
}

// ActiveSessionIDs implements agent.DownstreamHandler.
func (h *Handler) ActiveSessionIDs() []string { return h.sessions.activeIDs() }
func (h *Handler) ActiveToolRuns() []*agentcomposev2.NodeActiveToolRun {
	return h.toolruns.ActiveRuns()
}

// newIosFrameHandler wires the iOS device manager for this execution node, or
// returns nil when the host does not offer the capability.
//
// The device list lives in its own directory (agent-compose/ios), the same one
// the standalone node-ios binary uses: the config format is identical, so an
// operator can move between the two shapes, and it stays clear of the execution
// node's own config/lock in agent-compose/node. nodeID is stamped into each
// device's register frame so the server joins device ↔ node.
//
// ctx is the process-lifetime context: the tunnel agent and the device loops
// must outlive a control-stream drop, so they are not tied to the client's
// per-stream context.
func newIosFrameHandler(ctx context.Context, c *agent.Client, nodeID string, allowed bool) *ioshost.FrameHandler {
	if !allowed {
		return nil
	}
	logger := c.Logger()
	cfg, path, err := ioshost.LoadDevicesConfig("")
	if err != nil {
		// A broken devices.json must not take the node down: the capability was
		// advertised, so serve the frames but start from an empty inventory.
		logger.Warn("ios: devices config unreadable; starting with none", "error", err)
		cfg, path = &ioshost.DevicesConfig{}, ""
	}
	// iOS 17+ only exposes testmanagerd (which launches the WDA XCTest runner)
	// through a CoreDevice RSD tunnel; go-ios's RunTestWithConfig refuses to
	// launch on 17+ unless the device entry carries an RSD provider, which only
	// a running tunnel agent supplies. The standalone node-ios binary starts
	// this agent in its main; an execution node driving an attached iPhone must
	// start the same agent here, or WDA Launch (and the device-control runner
	// loop) fails on 17+ with "no RSD tunnel". Reuses an external
	// `ios tunnel start` if one is already up. iOS ≤16 never touches it.
	ioshost.StartTunnelAgent(ctx, logger, ioshost.StateDir(path))
	manager := ioshost.NewDeviceManager(ioshost.ManagerConfig{
		Logger:        logger,
		ConfigPath:    path,
		DevicesConfig: cfg,
		NodeID:        nodeID,
		OnReport: func(rep *agentcomposev2.NodeIosDevicesReport) error {
			return c.EmitUpstream(&agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_IosDevicesReport{IosDevicesReport: rep},
			})
		},
	})
	jobs := ioshost.NewWdaJobManager(c.EmitUpstream, ioshost.NewGoiosWdaSteps(logger, c), ioshost.StateDir(path), logger)
	// Wire job progress/terminal outcome back into the device's wda_state so the
	// console renders "initializing 42%" / "ready" / "failed" from the durable
	// inventory — surviving a server restart (the job snapshot does not).
	jobs.OnProgress = manager.NoteWdaJobProgress
	jobs.OnTerminal = func(udid string, ok bool, cancelled bool, errCode string, profileExpiresAt string) {
		manager.NoteWdaJobResult(udid, ok, cancelled, errCode, profileExpiresAt)
	}
	// The device loops and the WDA job engine both outlive a control-stream drop;
	// the manager is started here and stopped by the client's ctx.
	go manager.Start(context.Background())
	go manager.StartClaimedDevices(context.Background())
	return ioshost.NewFrameHandler(manager, jobs)
}

// StopAll implements agent.DownstreamHandler: on a connection drop, stop the
// running editor processes but keep the sessions and their working dirs so a
// reconnect can resume. Host terminals and tunnel daemons survive: the next
// heartbeat advertises them for server-side reconciliation.
func (h *Handler) StopAll() {
	h.sessions.stopAll()
	h.terminals.DetachAll()
}

// HandleFrame implements agent.DownstreamHandler: it routes one downstream
// session command onto the session manager and acks through the client.
func (h *Handler) HandleFrame(ctx context.Context, c *agent.Client, frame *agentcomposev2.NodeDownstreamFrame) {
	if h.builds.HandleBuildFrame(ctx, c, frame) {
		return
	}
	if h.xcode.HandleHostToolJobFrame(ctx, c, frame) {
		return
	}
	// iOS device management frames (discover/claim/release/configure/WDA job/
	// runner control). nil on a host without the capability — the frame then
	// falls through to the switch below and is rejected there.
	if h.ios != nil && h.ios.HandleIosFrame(ctx, c, frame) {
		return
	}
	frameID := frame.GetServerFrameId()
	switch payload := frame.GetFrame().(type) {
	case *agentcomposev2.NodeDownstreamFrame_CreateSession:
		// create() registers a placeholder session and returns immediately (acking
		// the dispatch), then runs the heavy prep (clone, downloads) in a background
		// goroutine. This keeps the ack under 30s even for slow networks and large
		// repos with submodules. Early-arriving user input is buffered in the
		// placeholder and flushed once provisioning completes.
		c.SendAck(frameID, h.sessions.create(ctx, payload.CreateSession), nil)
	case *agentcomposev2.NodeDownstreamFrame_DeleteSession:
		c.SendAck(frameID, h.sessions.delete(payload.DeleteSession.GetSessionId()), nil)
	case *agentcomposev2.NodeDownstreamFrame_ListSessions:
		c.SendAck(frameID, nil, h.sessions.summaries())
	case *agentcomposev2.NodeDownstreamFrame_TunnelRequest:
		// Reverse-proxy tunnel: forward the HTTP request to the session's local
		// service and stream the response back up. Runs in its own goroutine so a
		// slow proxied service does not block the dispatch loop.
		go h.sessions.handleTunnel(ctx, payload.TunnelRequest, c.EmitUpstream)
	case *agentcomposev2.NodeDownstreamFrame_ProxyRequest:
		// Node-mode forward HTTP proxy: the server wants a channel/API request
		// to exit from this node's IP. The node performs the outbound request
		// itself (TLS terminated on the node) and streams the response back.
		// Runs in its own goroutine so a slow upstream does not block dispatch.
		go h.sessions.handleProxy(ctx, payload.ProxyRequest, c.EmitUpstream)
	case *agentcomposev2.NodeDownstreamFrame_SessionInput:
		// Multi-turn input for an interactive session: relay it to the running
		// provider stream process's stdin.
		h.sessions.deliverInput(payload.SessionInput)
	case *agentcomposev2.NodeDownstreamFrame_ConfigureSessionLlm:
		cfg := payload.ConfigureSessionLlm
		res, err := h.sessions.configureLLM(cfg.GetSessionId(), cfg.GetRevision(), cfg.GetLlm())
		sendConfigAck(c, frameID, err, res)
	case *agentcomposev2.NodeDownstreamFrame_ApplySessionMcps:
		cfg := payload.ApplySessionMcps
		res, err := h.sessions.applyMCPs(cfg.GetSessionId(), cfg.GetRevision(), cfg.GetMcps())
		sendConfigAck(c, frameID, err, res)
	case *agentcomposev2.NodeDownstreamFrame_ApplySessionSkills:
		cfg := payload.ApplySessionSkills
		res, err := h.sessions.applySkills(cfg.GetSessionId(), cfg.GetRevision(), cfg.GetSkills())
		sendConfigAck(c, frameID, err, res)
	case *agentcomposev2.NodeDownstreamFrame_ApplySessionPlugins:
		cfg := payload.ApplySessionPlugins
		res, err := h.sessions.applyPlugins(cfg.GetSessionId(), cfg.GetRevision(), cfg.GetPlugins())
		sendConfigAck(c, frameID, err, res)
	case *agentcomposev2.NodeDownstreamFrame_ConfigureSessionMode:
		cfg := payload.ConfigureSessionMode
		res, err := h.sessions.configureMode(cfg.GetSessionId(), cfg.GetRevision(), cfg.GetMode())
		sendConfigAck(c, frameID, err, res)
	case *agentcomposev2.NodeDownstreamFrame_StartSessionRuntime:
		c.SendAck(frameID, h.sessions.startRuntime(payload.StartSessionRuntime.GetSessionId()), nil)
	case *agentcomposev2.NodeDownstreamFrame_RestartSessionRuntime:
		cfg := payload.RestartSessionRuntime
		c.SendAck(frameID, h.sessions.restartRuntime(cfg.GetSessionId(), cfg.GetFresh()), nil)
	case *agentcomposev2.NodeDownstreamFrame_CollectSessionArtifacts:
		// Artifact collection is served through the file-service tunnel; the node
		// just confirms the session exists and the archive endpoint is up.
		c.SendAck(frameID, h.sessions.markArtifactsCollectable(payload.CollectSessionArtifacts.GetSessionId()), nil)
	case *agentcomposev2.NodeDownstreamFrame_ManageEditor:
		// Install/upgrade an editor CLI on this host. Runs in its own goroutine:
		// a global npm install takes tens of seconds and must not block the
		// dispatch loop (heartbeats and session commands keep flowing).
		//
		// Detached from the stream ctx: an install acks only on completion, so
		// inheriting the per-stream ctx meant a reconnect (server restart, link
		// blip) killed npm mid-install — "exit status 0xffffffff", no output,
		// and a useless install-failed error. See agent.DetachStreamContext.
		go func(spec *agentcomposev2.NodeManageEditor) {
			version, err := manageEditor(agent.DetachStreamContext(ctx), spec)
			c.SendEditorAck(frameID, err, version)
		}(payload.ManageEditor)
	case *agentcomposev2.NodeDownstreamFrame_InstallHostTool:
		// Install a host-level runtime dependency (currently Node.js). A full
		// Node.js tarball is ~30 MB; download/extract must not block heartbeats
		// or session dispatch. Detached from the stream ctx for the same reason
		// as ManageEditor: the install acks only after the download finishes.
		go func(spec *agentcomposev2.NodeInstallHostTool) {
			nodeV, npmV, xcodeV, err := agent.InstallHostTool(agent.DetachStreamContext(ctx), spec, c.Logger(), c.DownloadProxy())
			c.SendHostToolAck(frameID, err, nodeV, npmV, xcodeV)
		}(payload.InstallHostTool)
	case *agentcomposev2.NodeDownstreamFrame_ManageEnvironment:
		// Create/remove a named shared environment directory on this host. Disk
		// work only (mkdir/remove), so it is cheap, but run it off the dispatch
		// loop anyway so a slow filesystem cannot stall heartbeats.
		go func(spec *agentcomposev2.NodeManageEnvironment) {
			err := h.sessions.manageEnvironment(ctx, spec)
			c.SendAck(frameID, err, nil)
		}(payload.ManageEnvironment)
	case *agentcomposev2.NodeDownstreamFrame_SyncEnvironment:
		// Environment maintenance: download/lay out the desired skill+plugin set
		// in the env HOME. These are real downloads (same path as session resource
		// sync), so it must run off the dispatch loop — and detached from the
		// stream ctx, since the ack only lands once the downloads finish.
		go func(spec *agentcomposev2.NodeSyncEnvironment) {
			err := h.sessions.syncEnvironment(agent.DetachStreamContext(ctx), spec)
			c.SendAck(frameID, err, nil)
		}(payload.SyncEnvironment)
	case *agentcomposev2.NodeDownstreamFrame_InspectEnvironment:
		// Report what is physically installed in the env HOME so the server can
		// diff it against the configured set. Directory listing only.
		go func(spec *agentcomposev2.NodeInspectEnvironment) {
			inventory, err := h.sessions.inspectEnvironment(spec)
			c.SendEnvironmentInventoryAck(frameID, err, inventory)
		}(payload.InspectEnvironment)
	case *agentcomposev2.NodeDownstreamFrame_InspectSystemEnv:
		// Enumerate what the providers would discover in the OPERATOR's HOME
		// (system env). Read-only directory/config listing, but run off the
		// dispatch loop like the env variant so a slow home dir cannot stall
		// heartbeats.
		go func(spec *agentcomposev2.NodeInspectSystemEnv) {
			inventory, err := h.sessions.inspectSystemEnv(spec)
			c.SendSystemEnvInventoryAck(frameID, err, inventory)
		}(payload.InspectSystemEnv)
	case *agentcomposev2.NodeDownstreamFrame_SyncSystemEnv:
		// Install platform resources into the operator's HOME / remove
		// platform-installed ones. Real downloads, so off the dispatch loop. The
		// ack carries the per-entry outcome (installed/skipped/removed) and only
		// lands once the downloads finish, so it is detached from the stream ctx.
		go func(spec *agentcomposev2.NodeSyncSystemEnv) {
			touched, err := h.sessions.syncSystemEnv(agent.DetachStreamContext(ctx), spec)
			c.SendSystemEnvInventoryAck(frameID, err, touched)
		}(payload.SyncSystemEnv)
	case *agentcomposev2.NodeDownstreamFrame_ArchiveSystemEnvResource:
		// Tar one resource out of the operator's HOME and POST it back so it can
		// enter the platform library. Network + disk, so off the dispatch loop —
		// and detached from the stream ctx, since the ack waits on the upload.
		go func(spec *agentcomposev2.NodeArchiveSystemEnvResource) {
			err := h.sessions.archiveSystemEnvResource(agent.DetachStreamContext(ctx), spec)
			c.SendAck(frameID, err, nil)
		}(payload.ArchiveSystemEnvResource)
	case *agentcomposev2.NodeDownstreamFrame_SelfUpgrade:
		// Download a replacement binary and restart. Ack "accepted" first (the
		// connection drops when we restart, so a post-restart ack is impossible);
		// the server confirms success by the reconnect reporting the new version.
		c.Logger().Info("self-upgrade: received command from server",
			"frame_id", frameID,
			"target_version", payload.SelfUpgrade.GetTargetVersion(),
			"download_url", payload.SelfUpgrade.GetDownloadUrl())
		c.SendAck(frameID, nil, nil)
		go func(spec *agentcomposev2.NodeSelfUpgrade) {
			if err := agent.SelfUpgrade(agent.DetachStreamContext(ctx), spec, c.Logger(), c.DownloadProxy()); err != nil {
				if agent.IsRestartExit(err) {
					c.Logger().Info("self-upgrade: restarting into new binary")
					os.Exit(0)
				}
				c.Logger().Error("self-upgrade failed", "error", err)
			}
		}(payload.SelfUpgrade)
	case *agentcomposev2.NodeDownstreamFrame_RuntimeUpgrade:
		// Runtime replacement does not restart the Go node. Ack only after the
		// archive has been verified and activated so the service can refresh its
		// runtime_version label immediately. Detached from the stream ctx: the
		// download can take minutes, and a reconnect must not abort it (the
		// archive is swapped in on disk regardless, so killing it mid-way only
		// loses the ack and the version label).
		go func(spec *agentcomposev2.NodeRuntimeUpgrade) {
			err := agent.RuntimeUpgrade(agent.DetachStreamContext(ctx), spec, c.Logger(), c.DownloadProxy())
			if err != nil {
				// Previously only the ack carried the error, so a failed runtime
				// upgrade left nothing in the node log — the operator saw a
				// generic failure and the cause had to be reconstructed by hand.
				c.Logger().Error("runtime-upgrade failed", "error", err)
			}
			c.SendAck(frameID, err, nil)
		}(payload.RuntimeUpgrade)
	case *agentcomposev2.NodeDownstreamFrame_TerminalOpen:
		// Host-shell terminal for the management console. The manager runs its
		// own output pump goroutine, so this returns immediately and never
		// stalls the dispatch loop.
		//
		// When the open frame names a session, resolve that session's workspace
		// dir on the node and open the shell there — the server never learns (or
		// dictates) the node-local absolute path. An unknown session falls back
		// to the frame's explicit cwd (else node home) rather than failing.
		//
		// An env_id instead opens a MAINTENANCE shell inside a shared
		// environment: the node resolves the env dir and points the shell's HOME
		// at it, so installing deps or logging a CLI in lands in the environment
		// rather than the operator's own home. env_id wins over session_id — a
		// maintenance shell targets the environment, not a session.
		spec := payload.TerminalOpen
		var extraEnv []string
		if envID := strings.TrimSpace(spec.GetEnvId()); envID != "" {
			dir := h.sessions.envDir(envID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				c.Logger().Error("environment terminal: prepare dir failed", "env_id", envID, "error", err)
			} else {
				spec.Cwd = dir
				extraEnv = []string{"HOME=" + dir, "USERPROFILE=" + dir}
			}
		} else if sid := spec.GetSessionId(); sid != "" {
			if dir := h.sessions.workspaceDir(sid); dir != "" {
				spec.Cwd = dir
			}
		}
		h.terminals.Open(spec, extraEnv...)
	case *agentcomposev2.NodeDownstreamFrame_TerminalInput:
		h.terminals.Input(payload.TerminalInput)
	case *agentcomposev2.NodeDownstreamFrame_TerminalResize:
		h.terminals.Resize(payload.TerminalResize)
	case *agentcomposev2.NodeDownstreamFrame_TerminalClose:
		h.terminals.Close(payload.TerminalClose)
	case *agentcomposev2.NodeDownstreamFrame_TerminalAttach:
		// Resume streaming an existing terminal after a NodeConnect reconnect.
		// The node keeps its PTYs alive across a dropped control stream, so a
		// reconnect reattaches rather than opening a fresh shell (which would
		// lose cwd and any running foreground process).
		h.terminals.Attach(payload.TerminalAttach)
	case *agentcomposev2.NodeDownstreamFrame_TerminalList:
		// Management console: report every host terminal and what it is running.
		h.terminals.List(payload.TerminalList)
	case *agentcomposev2.NodeDownstreamFrame_TerminalInterrupt:
		// Management console: interrupt the running command, keep the terminal.
		h.terminals.Interrupt(payload.TerminalInterrupt)
	case *agentcomposev2.NodeDownstreamFrame_HostExec:
		// Host commands run outside the dispatch loop and return a structured
		// result correlated by request_id (a slow command must not stall heartbeats).
		go func(req *agentcomposev2.NodeHostExecRequest) {
			result := agent.RunHostExec(ctx, req)
			_ = c.EmitUpstream(&agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_HostExecResult{HostExecResult: result},
			})
		}(payload.HostExec)
	case *agentcomposev2.NodeDownstreamFrame_FileUpload:
		// Upload chunks write to a temp file off the dispatch loop; the ack is
		// correlated by upload_id so the server can pipeline the next chunk.
		go func(req *agentcomposev2.NodeFileUploadRequest) {
			result := agent.RunFileUpload(ctx, req)
			_ = c.EmitUpstream(&agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_FileUploadResult{FileUploadResult: result},
			})
		}(payload.FileUpload)
	case *agentcomposev2.NodeDownstreamFrame_ToolRunRequest:
		// Long-running external CLI (tunnel manager). Start is non-blocking;
		// output + exit are streamed as NodeToolRunEvent frames by run_id.
		h.toolruns.Start(payload.ToolRunRequest)
	case *agentcomposev2.NodeDownstreamFrame_ToolRunStop:
		h.toolruns.Stop(payload.ToolRunStop)
	default:
		c.Logger().Warn("unexpected downstream frame for execution node", "type", fmt.Sprintf("%T", frame.GetFrame()))
	}
}

// sendConfigAck bridges a configResult onto the client's config-ack primitive.
func sendConfigAck(c *agent.Client, frameID string, cmdErr error, res configResult) {
	c.SendConfigAck(frameID, cmdErr, res.appliedRevision, res.effectiveRevision, res.restartRequired)
}
