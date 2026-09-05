// Command agent-compose-node-management is the management node binary. It dials
// the agent-compose server outbound and holds a single long-lived bidirectional
// stream (NodeService.NodeConnect): the server pushes CreateExecutionNode /
// DeleteExecutionNode commands, and this node launches or tears down execution
// nodes on its host in response. It runs no provider sessions itself — the
// execution nodes it launches dial the server directly and are not in this
// node's data path.
//
// This is one of the two node binaries split out of the former single
// agent-compose-agent (which selected its role with --role). The role is fixed
// here: management. The connection layer is shared with the execution binary via
// ai-lubricant-nodes/common/agent.
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

// buildVersion is overridden at link time; it mirrors the daemon convention.
var buildVersion = "dev"

type options struct {
	agent        agent.Options
	agentImage   string
	executionBin string
}

func main() {
	opts, instance, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, agent.ErrInstallCancelled) {
			fmt.Fprintln(os.Stderr, "agent-compose-node-management: install cancelled; existing node was left unchanged")
			return
		}
		if errors.Is(err, agent.ErrInstallComplete) {
			fmt.Fprintln(os.Stderr, "agent-compose-node-management: node configuration installed")
			return
		}
		if errors.Is(err, agent.ErrAlreadyRunning) {
			fmt.Fprintln(os.Stderr, "agent-compose-node-management: another Agent Compose node is already running on this machine")
			return
		}
		fmt.Fprintln(os.Stderr, "agent-compose-node-management:", err)
		os.Exit(2)
	}
	defer instance.Close()

	// Same as the execution binary: log to stderr plus a rotating file under the
	// install dir. The file name carries the binary, so the two roles sharing one
	// install dir never write to the same file.
	logger := agent.SetupLogger("info")
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("agent-compose-node-management starting",
		"version", buildVersion,
		"server", opts.agent.Server,
		"node_name", opts.agent.NodeName)

	client := agent.NewClient(opts.agent, logger)
	client.SetHandler(NewHandler(client, opts.agent.Server, opts.agentImage, opts.executionBin))

	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("agent-compose-node-management exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("agent-compose-node-management stopped")
}

func parseFlags(args []string) (options, *agent.Instance, error) {
	fs := flag.NewFlagSet("agent-compose-node-management", flag.ContinueOnError)
	var (
		server       = fs.String("server", agent.EnvOr("AGENT_COMPOSE_SERVER", ""), "server base URL, e.g. https://host:7410 or http://host:7410 (env AGENT_COMPOSE_SERVER)")
		nodeID       = fs.String("node-id", agent.EnvOr("AGENT_COMPOSE_NODE_ID", ""), "durable node id minted at onboard (env AGENT_COMPOSE_NODE_ID)")
		secret       = fs.String("secret", agent.EnvOr("AGENT_COMPOSE_NODE_SECRET", ""), "node TOTP secret (base32) minted at onboard (env AGENT_COMPOSE_NODE_SECRET)")
		nodeName     = fs.String("name", agent.EnvOr("AGENT_COMPOSE_NODE_NAME", ""), "human-readable node name (env AGENT_COMPOSE_NODE_NAME); default is the host name")
		agentImage   = fs.String("agent-image", agent.EnvOr("AGENT_COMPOSE_AGENT_IMAGE", ""), "container image this node launches execution nodes from (env AGENT_COMPOSE_AGENT_IMAGE)")
		executionBin = fs.String("execution-bin", agent.EnvOr("AGENT_COMPOSE_EXECUTION_BIN", ""), "path to the execution-node binary for standalone launches (env AGENT_COMPOSE_EXECUTION_BIN); empty resolves agent-compose-node-execution on PATH")
		labels       = fs.String("labels", agent.EnvOr("AGENT_COMPOSE_NODE_LABELS", ""), "comma-separated key=value capability labels")
		tlsInsecure  = fs.Bool("tls-insecure", agent.EnvOr("AGENT_COMPOSE_NODE_TLS_INSECURE", "") == "1", "skip TLS certificate verification (dev only)")
		heartbeat    = fs.Duration("heartbeat", 15*time.Second, "heartbeat interval")
		install      = fs.Bool("install", false, "save the supplied credentials on this machine as the single local node, replacing any previous one after confirmation")
		installOnly  = fs.Bool("install-only", false, "with --install, save the configuration and exit instead of staying in the foreground")
		assumeYes    = fs.Bool("yes", false, "with --install, approve replacing an existing local node without prompting (automation only)")
	)
	if err := fs.Parse(args); err != nil {
		return options{}, nil, err
	}
	resolved, err := agent.ResolveLocalConfig(agent.LocalConfig{
		Server:       *server,
		NodeID:       *nodeID,
		Secret:       *secret,
		Role:         agent.RoleManagement,
		NodeName:     strings.TrimSpace(*nodeName),
		AgentImage:   strings.TrimSpace(*agentImage),
		ExecutionBin: strings.TrimSpace(*executionBin),
		Labels:       strings.TrimSpace(*labels),
		TLSInsecure:  *tlsInsecure,
	}, agent.InstallOptions{Install: *install, AssumeYes: *assumeYes})
	if err != nil {
		return options{}, nil, err
	}
	cfg := resolved.Config

	instance, err := agent.AcquireInstance(cfg, resolved.Rebind)
	if err != nil {
		return options{}, nil, err
	}
	if resolved.Persist {
		if err := agent.SaveLocalConfig(&cfg); err != nil {
			instance.Close()
			return options{}, nil, err
		}
	}
	if *installOnly {
		instance.Close()
		return options{}, nil, agent.ErrInstallComplete
	}
	name := cfg.NodeName
	if name == "" {
		name = agent.DefaultNodeName()
	}
	// A management node does not run provider CLIs itself; it only launches
	// execution nodes. It still advertises its docker capability so the server
	// knows it can launch containerized execution nodes.
	return options{
		agent: agent.Options{
			Server:      cfg.Server,
			NodeID:      cfg.NodeID,
			Secret:      cfg.Secret,
			NodeName:    name,
			Role:        agent.RoleManagement,
			Version:     buildVersion,
			Labels:      agent.ParseLabels(cfg.Labels),
			Docker:      agent.DockerAvailable(),
			TLSInsecure: cfg.TLSInsecure,
			MinBackoff:  time.Second,
			MaxBackoff:  30 * time.Second,
			Heartbeat:   *heartbeat,
		},
		agentImage:   cfg.AgentImage,
		executionBin: cfg.ExecutionBin,
	}, instance, nil
}
