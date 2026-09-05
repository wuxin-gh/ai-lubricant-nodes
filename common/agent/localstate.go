package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// InstallOptions controls a credentialed install/rebind operation.
type InstallOptions struct {
	Install   bool
	AssumeYes bool
	In        io.Reader
	Out       io.Writer
}

// ResolvedConfig describes the selected runtime config and whether it should be
// committed after the host lock has been acquired. Deferring the write keeps an
// approved rebind atomic: the old process is stopped before its config changes.
type ResolvedConfig struct {
	Config  LocalConfig
	Persist bool
	Rebind  bool
}

// ResolveLocalConfig selects persisted credentials or validates a proposed
// install/rebind. It does not write: the caller acquires the host lock first,
// then commits Config when Persist is true.
func ResolveLocalConfig(candidate LocalConfig, opt InstallOptions) (ResolvedConfig, error) {
	old, err := LoadLocalConfig()
	if err != nil {
		return ResolvedConfig{}, err
	}
	candidate.Server = strings.TrimRight(strings.TrimSpace(candidate.Server), "/")
	candidate.NodeID = strings.TrimSpace(candidate.NodeID)
	candidate.Secret = strings.TrimSpace(candidate.Secret)
	candidate.Role = strings.TrimSpace(candidate.Role)
	// An explicit --yes is an automation-approved install, so it carries install
	// intent on its own (docker/systemd images cannot answer a prompt).
	install := opt.Install || opt.AssumeYes

	// No credentials supplied: the normal "just start it again" path.
	if candidate.Server == "" && candidate.NodeID == "" && candidate.Secret == "" {
		if old == nil {
			return ResolvedConfig{}, fmt.Errorf("node is not installed on this machine; run the installer once with --install --server ... --node-id ... --secret ...")
		}
		if candidate.Role != "" && old.Role != candidate.Role {
			return ResolvedConfig{}, fmt.Errorf("this machine is installed as a %s node; start the %s node binary instead", old.Role, old.Role)
		}
		return ResolvedConfig{Config: *old}, nil
	}
	if candidate.Server == "" || candidate.NodeID == "" || candidate.Secret == "" {
		return ResolvedConfig{}, fmt.Errorf("--server, --node-id and --secret must be supplied together")
	}

	// Re-running the same install is idempotent: reuse the stored extras and
	// rewrite the same content rather than prompting or clearing anything.
	if old.SameCredentials(candidate.Server, candidate.NodeID, candidate.Secret, candidate.Role) {
		mergeLocalExtras(&candidate, old)
		return ResolvedConfig{Config: candidate, Persist: install}, nil
	}

	if old == nil {
		return ResolvedConfig{Config: candidate, Persist: install}, nil
	}
	if !install {
		return ResolvedConfig{}, fmt.Errorf(
			"supplied credentials differ from the node installed here (%s); rerun with --install to replace it",
			old.Describe())
	}
	ok, err := confirmRebind(old, &candidate, opt)
	if err != nil {
		return ResolvedConfig{}, err
	}
	if !ok {
		return ResolvedConfig{}, ErrInstallCancelled
	}
	return ResolvedConfig{Config: candidate, Persist: true, Rebind: true}, nil
}

func mergeLocalExtras(dst *LocalConfig, src *LocalConfig) {
	if dst.NodeName == "" {
		dst.NodeName = src.NodeName
	}
	if dst.WorkRoot == "" {
		dst.WorkRoot = src.WorkRoot
	}
	if dst.Providers == "" {
		dst.Providers = src.Providers
	}
	if dst.AgentImage == "" {
		dst.AgentImage = src.AgentImage
	}
	if dst.ExecutionBin == "" {
		dst.ExecutionBin = src.ExecutionBin
	}
	if dst.Labels == "" {
		dst.Labels = src.Labels
	}
}

// ErrInstallCancelled means the operator declined a local rebind. It is not a
// failure and callers should exit without changing config/processes.
var ErrInstallCancelled = errors.New("node install cancelled")

func confirmRebind(old, next *LocalConfig, opt InstallOptions) (bool, error) {
	if opt.AssumeYes {
		return true, nil
	}
	in, out := opt.In, opt.Out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if !inputIsTerminal(in) {
		// Refuse to stop a running node on a non-interactive host even if
		// credentials differ: an operator may have piped them in. They must
		// surface approval explicitly (--yes) for an automated rebind.
		return false, fmt.Errorf("existing node data found; interactive confirmation is required (or pass --yes with --install for approved automation)")
	}
	fmt.Fprintln(out, "An Agent Compose node is already installed on this machine:")
	fmt.Fprintln(out, "  old:", old.Describe())
	fmt.Fprintln(out, "  new:", next.Describe())
	fmt.Fprintln(out, "This will stop the old local node and replace its local credentials.")
	fmt.Fprintln(out, "Server records and session work directories will not be deleted.")
	fmt.Fprint(out, "Continue? [y/N]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func inputIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return true // injected readers in tests are intentionally interactive
	}
	return fdIsTerminal(f.Fd())
}

// Instance is the held host-wide node lock. Close is idempotent so a setup
// failure can release early while the caller's deferred Close remains safe.
type Instance struct {
	once    sync.Once
	release func() error
	err     error
}

func (i *Instance) Close() error {
	if i == nil || i.release == nil {
		return nil
	}
	i.once.Do(func() { i.err = i.release() })
	return i.err
}

// AcquireInstance enforces one manually installed node process for the whole
// machine. An approved rebind (replace=true) may stop a verified old node so the
// new one can take over; otherwise an existing lock is a hard error.
func AcquireInstance(cfg LocalConfig, replace bool) (*Instance, error) {
	release, held, err := tryPlatformLock()
	if err != nil {
		return nil, err
	}
	if held {
		if err := writeOwner(cfg); err != nil {
			_ = release()
			return nil, err
		}
		return &Instance{release: func() error { _ = removeOwner(); return release() }}, nil
	}
	if !replace {
		return nil, ErrAlreadyRunning
	}
	owner, _ := readOwner()
	if !trustedOwner(owner) {
		return nil, fmt.Errorf("another node holds the host lock but its process could not be verified; stop it manually before rebinding")
	}
	if err := stopProcess(owner.PID); err != nil {
		return nil, fmt.Errorf("stop old node pid %d: %w", owner.PID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		release, held, err = tryPlatformLock()
		if err != nil {
			return nil, err
		}
		if held {
			if err := writeOwner(cfg); err != nil {
				_ = release()
				return nil, err
			}
			return &Instance{release: func() error { _ = removeOwner(); return release() }}, nil
		}
	}
	return nil, fmt.Errorf("old node pid %d did not release the host lock", owner.PID)
}

var (
	ErrAlreadyRunning = errors.New("an Agent Compose node is already running on this machine")
	// ErrInstallComplete is returned by --install-only after credentials have
	// been committed and the host lock released. Mains treat it as a clean exit.
	ErrInstallComplete = errors.New("node installation complete")
)

type ownerInfo struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	NodeID     string `json:"node_id"`
	Role       string `json:"role"`
}

func ownerPath() (string, error) { d, e := stateDir(); return filepath.Join(d, "owner.json"), e }
