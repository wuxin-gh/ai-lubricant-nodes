// Command node-execution is the execution node agent. It dials the agent-compose
// server outbound and holds a single long-lived bidirectional stream
// (NodeService.NodeConnect): the agent sends registration, heartbeats, session
// output, and results; the server pushes session commands (create/delete/list)
// back down the same stream.
//
// The agent is stateless across restarts: it holds only the sessions it is
// currently running plus their local working directories. On reconnect it
// re-registers and resumes taking work. It runs provider CLIs (claude/codex/…)
// either directly as local processes or inside a docker container.
//
// This binary is execution-only; it has no --role flag. Management nodes are a
// separate binary (ai-lubricant-nodes/management).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ai-lubricant-nodes/common/agent"
)

// buildVersion is overridden at link time.
var buildVersion = "dev"

func main() {
	opts, workRoot, instance, systemEnvAllowed, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, agent.ErrInstallCancelled) {
			fmt.Fprintln(os.Stderr, "node-execution: install cancelled; existing node was left unchanged")
			return
		}
		if errors.Is(err, agent.ErrInstallComplete) {
			fmt.Fprintln(os.Stderr, "node-execution: node configuration installed")
			return
		}
		if errors.Is(err, agent.ErrAlreadyRunning) {
			fmt.Fprintln(os.Stderr, "node-execution: another Agent Compose node is already running on this machine")
			return
		}
		fmt.Fprintln(os.Stderr, "node-execution:", err)
		os.Exit(2)
	}
	defer instance.Close()

	// SetupLogger wires stderr AND a rotating file in the install dir, so a node
	// that misbehaves in production has a durable log to read. Building a bare
	// stderr-only handler here (as this used to) meant the file logging added to
	// the agent package was dead code — the reason the node's logs/ dir stayed
	// empty. Level is Info; the file is <install-dir>/logs/node-execution-<date>.log.
	logger := agent.SetupLogger("info")
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("node-execution starting",
		"version", buildVersion,
		"server", opts.Server,
		"node_name", opts.NodeName,
		"work_root", workRoot,
		"providers", strings.Join(opts.Providers, ","))

	client := agent.NewClient(opts, logger)
	client.SetHandler(NewHandler(client, workRoot, opts.Providers, opts.Docker, systemEnvAllowed))

	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("node-execution exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("node-execution stopped")
}

// parseFlags resolves the shared connection options plus the execution-only work
// root, honouring this precedence: explicit flags/env > persisted local config.
// It then takes the host-wide single-instance lock, so at most one manually
// installed node runs per machine. Credentials are only persisted in --install
// mode, and a rebind is confirmed before the old node is stopped.
func parseFlags(args []string) (agent.Options, string, *agent.Instance, bool, error) {
	fs := flag.NewFlagSet("node-execution", flag.ContinueOnError)
	var (
		server      = fs.String("server", agent.EnvOr("AGENT_COMPOSE_SERVER", ""), "server base URL, e.g. https://host:7410 or http://host:7410 (env AGENT_COMPOSE_SERVER)")
		nodeID      = fs.String("node-id", agent.EnvOr("AGENT_COMPOSE_NODE_ID", ""), "durable node id minted at onboard (env AGENT_COMPOSE_NODE_ID)")
		secret      = fs.String("secret", agent.EnvOr("AGENT_COMPOSE_NODE_SECRET", ""), "node TOTP secret (base32) minted at onboard (env AGENT_COMPOSE_NODE_SECRET)")
		nodeName    = fs.String("name", agent.EnvOr("AGENT_COMPOSE_NODE_NAME", ""), "human-readable node name (env AGENT_COMPOSE_NODE_NAME); default is the host name")
		workRoot    = fs.String("work-root", agent.EnvOr("AGENT_COMPOSE_NODE_WORK_ROOT", ""), "local directory for session working trees (env AGENT_COMPOSE_NODE_WORK_ROOT)")
		providers   = fs.String("providers", agent.EnvOr("AGENT_COMPOSE_NODE_PROVIDERS", ""), "comma-separated provider CLIs available on this node; empty = auto-detect")
		labels      = fs.String("labels", agent.EnvOr("AGENT_COMPOSE_NODE_LABELS", ""), "comma-separated key=value capability labels")
		tlsInsecure = fs.Bool("tls-insecure", agent.EnvOr("AGENT_COMPOSE_NODE_TLS_INSECURE", "") == "1", "skip TLS certificate verification (dev only)")
		heartbeat   = fs.Duration("heartbeat", 15*time.Second, "heartbeat interval")
		dockerFlag  = fs.String("docker", agent.EnvOr("AGENT_COMPOSE_NODE_DOCKER", "auto"), "offer the docker driver: auto|on|off (auto = detect the docker CLI)")
		install     = fs.Bool("install", false, "save the supplied credentials on this machine as the single local node, replacing any previous one after confirmation")
		installOnly = fs.Bool("install-only", false, "with --install, save the configuration and exit instead of staying in the foreground")
		assumeYes   = fs.Bool("yes", false, "with --install, approve replacing an existing local node without prompting (automation only)")
		managed     = fs.Bool("managed", agent.EnvOr("AGENT_COMPOSE_NODE_MANAGED", "") == "1", "this node is launched and tracked by a management node; skip the host single-instance lock and persisted config (env AGENT_COMPOSE_NODE_MANAGED)")
		// allowSystemEnv opts the node into env_mode="system" sessions, where an
		// editor runs against the operator's real HOME (installed toolchain, CLI
		// logins). auto (the default) allows on a host install and refuses inside
		// a container — the mode hands the whole home (ssh keys, provider
		// credentials) to any task the node runs, and a container's HOME is the
		// image's, not the operator's. on/off force it either way.
		allowSystemEnv = fs.String("allow-system-env", agent.EnvOr("AGENT_COMPOSE_NODE_ALLOW_SYSTEM_ENV", "auto"), "allow sessions to run against this node's real user HOME (env_mode=system): auto|on|off (auto = on for a host install, off inside a container) (env AGENT_COMPOSE_NODE_ALLOW_SYSTEM_ENV)")
	)
	if err := fs.Parse(args); err != nil {
		return agent.Options{}, "", nil, false, err
	}
	// Resolve once here so both launch paths and the handler share one verdict;
	// an invalid explicit value is a usage error, not a silent fallback.
	systemEnvAllowed, err := agent.ResolveSystemEnvCapability(*allowSystemEnv)
	if err != nil {
		return agent.Options{}, "", nil, false, err
	}

	// A managed child (launched by a management node) runs straight from its
	// flags/env: no host lock, no persisted config. The manual single-instance
	// rule applies only to nodes an operator installs directly on a machine.
	if *managed {
		if strings.TrimSpace(*server) == "" || strings.TrimSpace(*nodeID) == "" || strings.TrimSpace(*secret) == "" {
			return agent.Options{}, "", nil, false, fmt.Errorf("--server, --node-id and --secret are required")
		}
		providerList := agent.SplitAndTrim(*providers)
		if len(providerList) == 0 {
			providerList = detectProviders()
		}
		dockerEnabled, err := agent.ResolveDockerCapability(*dockerFlag)
		if err != nil {
			return agent.Options{}, "", nil, false, err
		}
		name := strings.TrimSpace(*nodeName)
		if name == "" {
			name = agent.DefaultNodeName()
		}
		opts := agent.Options{
			Server:           strings.TrimRight(strings.TrimSpace(*server), "/"),
			NodeID:           strings.TrimSpace(*nodeID),
			Secret:           strings.TrimSpace(*secret),
			NodeName:         name,
			Role:             agent.RoleExecution,
			Version:          buildVersion,
			Providers:        providerList,
			Labels:           agent.ParseLabels(*labels),
			TLSInsecure:      *tlsInsecure,
			Docker:           dockerEnabled,
			SystemEnvAllowed: systemEnvAllowed,
			MinBackoff:       time.Second,
			MaxBackoff:       30 * time.Second,
			Heartbeat:        *heartbeat,
		}
		return opts, managedWorkRoot(*workRoot), nil, systemEnvAllowed, nil
	}

	resolved, err := agent.ResolveLocalConfig(agent.LocalConfig{
		Server:      *server,
		NodeID:      *nodeID,
		Secret:      *secret,
		Role:        agent.RoleExecution,
		NodeName:    strings.TrimSpace(*nodeName),
		WorkRoot:    strings.TrimSpace(*workRoot),
		Providers:   strings.TrimSpace(*providers),
		Labels:      strings.TrimSpace(*labels),
		TLSInsecure: *tlsInsecure,
	}, agent.InstallOptions{Install: *install, AssumeYes: *assumeYes})
	if err != nil {
		return agent.Options{}, "", nil, false, err
	}
	cfg := resolved.Config

	// Take the host lock before persisting: an approved rebind must stop the old
	// node first so the two never race over the same credentials on disk.
	instance, err := agent.AcquireInstance(cfg, resolved.Rebind)
	if err != nil {
		return agent.Options{}, "", nil, false, err
	}
	if resolved.Persist {
		if err := agent.SaveLocalConfig(&cfg); err != nil {
			instance.Close()
			return agent.Options{}, "", nil, false, err
		}
	}
	if *installOnly {
		instance.Close()
		return agent.Options{}, "", nil, false, agent.ErrInstallComplete
	}

	providerList := agent.SplitAndTrim(cfg.Providers)
	if len(providerList) == 0 {
		providerList = detectProviders()
	}
	dockerEnabled, err := agent.ResolveDockerCapability(*dockerFlag)
	if err != nil {
		instance.Close()
		return agent.Options{}, "", nil, false, err
	}
	name := cfg.NodeName
	if name == "" {
		name = agent.DefaultNodeName()
	}
	root := cfg.WorkRoot
	if root == "" {
		root = defaultWorkRoot()
	}
	opts := agent.Options{
		Server:           cfg.Server,
		NodeID:           cfg.NodeID,
		Secret:           cfg.Secret,
		NodeName:         name,
		Role:             agent.RoleExecution,
		Version:          buildVersion,
		Providers:        providerList,
		Labels:           agent.ParseLabels(cfg.Labels),
		TLSInsecure:      cfg.TLSInsecure,
		Docker:           dockerEnabled,
		SystemEnvAllowed: systemEnvAllowed,
		MinBackoff:       time.Second,
		MaxBackoff:       30 * time.Second,
		Heartbeat:        *heartbeat,
	}
	return opts, root, instance, systemEnvAllowed, nil
}

func managedWorkRoot(value string) string {
	if root := strings.TrimSpace(value); root != "" {
		return root
	}
	return defaultWorkRoot()
}

func defaultWorkRoot() string {
	if dir, err := os.UserCacheDir(); err == nil && strings.TrimSpace(dir) != "" {
		return dir + string(os.PathSeparator) + "agent-compose-node"
	}
	return "." + string(os.PathSeparator) + "agent-compose-node"
}

// detectProviders probes PATH for the known provider CLIs so a freshly installed
// node advertises what it can actually run without extra config.
func detectProviders() []string {
	var found []string
	for _, name := range []string{"claude", "codex", "gemini", "opencode", "agent"} {
		if _, err := agent.LookPath(name); err == nil {
			if name == "agent" {
				// Cursor's CLI binary is named ``agent``; only advertise cursor when
				// it is present so a bare ``agent`` shim of another tool does not
				// register a provider we cannot drive.
				found = append(found, "cursor")
			} else {
				found = append(found, name)
			}
		}
	}
	return found
}
