package sshx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	posixpath "path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type RawResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
}

type HostInfo struct {
	Hostname     string `json:"hostname"`
	Kernel       string `json:"kernel"`
	Architecture string `json:"architecture"`
	User         string `json:"user"`
	Uptime       string `json:"uptime"`
}

type HostKey struct {
	Lines       string `json:"lines"`
	Fingerprint string `json:"fingerprint"`
	Algorithm   string `json:"algorithm,omitempty"`
	Trusted     bool   `json:"trusted"`
}

// ConnectionSpec is resolved by the control plane. Jumps are ordered from the
// first reachable bastion to the target's immediate bastion.
type ConnectionSpec struct {
	Target domain.Host
	Jumps  []domain.Host
}

type Transport interface {
	Exec(context.Context, ConnectionSpec, domain.ExecRequest) (RawResult, error)
	Probe(context.Context, ConnectionSpec) (HostInfo, error)
	ScanHostKey(context.Context, ConnectionSpec) (HostKey, error)
	TrustHostKey(context.Context, ConnectionSpec, string) (HostKey, error)
	StoredHostKey(domain.Host) (HostKey, bool)
}

type StreamingTransport interface {
	ExecStream(context.Context, ConnectionSpec, domain.ExecRequest, func(stream string, data []byte)) (RawResult, error)
}

type ShellExit struct {
	ExitCode *int
	Signal   string
	Err      error
}

// ShellSession is one persistent PTY-backed SSH process. The control plane
// owns its lifetime and is responsible for binding writes to an approved
// Agent conversation.
type ShellSession interface {
	Write([]byte) (int, error)
	Resize(cols, rows int) error
	Interrupt() error
	Wait() ShellExit
	Close() error
}

type InteractiveTransport interface {
	OpenShell(context.Context, ConnectionSpec, domain.ExecRequest, int, int, func(stream string, data []byte)) (ShellSession, error)
}

type HostFileTransferTransport interface {
	TransferFile(context.Context, ConnectionSpec, ConnectionSpec, domain.ExecRequest, func(transferredBytes, totalBytes int64)) (RawResult, error)
}

// TunnelClient is one authenticated SSH connection that can open direct TCP
// channels through the configured target, ProxyJump chain, and network proxy.
type TunnelClient interface {
	Dial(network, address string) (net.Conn, error)
	Wait() error
	Close() error
}

// ReverseTunnelClient can request a listener from the SSH server for remote
// (-R) port forwarding. Accepted connections arrive through the SSH channel.
type ReverseTunnelClient interface {
	Listen(network, address string) (net.Listener, error)
}

type TunnelTransport interface {
	OpenTunnel(context.Context, ConnectionSpec) (TunnelClient, error)
}

const probeScript = `probe_hostname=""
if command -v hostname >/dev/null 2>&1; then probe_hostname=$(hostname 2>/dev/null) || probe_hostname=""; fi
if [ -z "$probe_hostname" ]; then IFS= read -r probe_hostname </proc/sys/kernel/hostname || probe_hostname="unknown"; fi
printf '%s\n' "${probe_hostname:-unknown}"

probe_kernel=""
if command -v uname >/dev/null 2>&1; then probe_kernel=$(uname -sr 2>/dev/null) || probe_kernel=""; fi
if [ -z "$probe_kernel" ]; then
  probe_ostype=""; probe_release=""
  IFS= read -r probe_ostype </proc/sys/kernel/ostype || probe_ostype="Linux"
  IFS= read -r probe_release </proc/sys/kernel/osrelease || probe_release="unknown"
  probe_kernel="$probe_ostype $probe_release"
fi
printf '%s\n' "$probe_kernel"

probe_arch=""
if command -v uname >/dev/null 2>&1; then probe_arch=$(uname -m 2>/dev/null) || probe_arch=""; fi
printf '%s\n' "${probe_arch:-${HOSTTYPE:-unknown}}"

probe_user=""
if command -v whoami >/dev/null 2>&1; then probe_user=$(whoami 2>/dev/null) || probe_user=""; fi
if [ -z "$probe_user" ] && command -v id >/dev/null 2>&1; then probe_user=$(id -un 2>/dev/null) || probe_user=""; fi
printf '%s\n' "${probe_user:-${USER:-${LOGNAME:-unknown}}}"

probe_uptime=""
if command -v uptime >/dev/null 2>&1; then probe_uptime=$(uptime -p 2>/dev/null) || probe_uptime=$(uptime 2>/dev/null) || probe_uptime=""; fi
if [ -z "$probe_uptime" ]; then
  probe_seconds=""
  IFS=' ' read -r probe_seconds _ </proc/uptime || probe_seconds="unknown"
  probe_uptime="up ${probe_seconds:-unknown} seconds"
fi
printf '%s\n' "$probe_uptime"`

func parseProbeOutput(output []byte) (HostInfo, error) {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 5 {
		return HostInfo{}, fmt.Errorf("unexpected probe output")
	}
	return HostInfo{Hostname: lines[0], Kernel: lines[1], Architecture: lines[2], User: lines[3], Uptime: strings.Join(lines[4:], "\n")}, nil
}

func buildRemoteCommand(req domain.ExecRequest) (string, io.Reader, error) {
	if req.Mode == "" {
		if req.Script != "" {
			req.Mode = domain.ExecScript
		} else {
			req.Mode = domain.ExecProgram
		}
	}
	prefix, err := remotePrefix(req.Cwd, req.Env)
	if err != nil {
		return "", nil, err
	}
	switch req.Mode {
	case domain.ExecProgram:
		if !regexp.MustCompile(`^[A-Za-z0-9_./+:-]+$`).MatchString(req.Program) {
			return "", nil, fmt.Errorf("program contains unsupported characters")
		}
		parts := []string{shellQuote(req.Program)}
		for _, arg := range req.Args {
			parts = append(parts, shellQuote(arg))
		}
		return prefix + strings.Join(parts, " "), nil, nil
	case domain.ExecScript:
		if strings.TrimSpace(req.Script) == "" {
			return "", nil, fmt.Errorf("script is empty")
		}
		return prefix + "bash -s", strings.NewReader(req.Script), nil
	default:
		return "", nil, fmt.Errorf("unsupported execution mode %q", req.Mode)
	}
}

func applyElevation(host domain.Host, req domain.ExecRequest, command string, stdin io.Reader) (string, io.Reader, error) {
	if !req.Elevated {
		return command, stdin, nil
	}
	wrapped := "bash -c " + shellQuote(command)
	switch host.SudoMode {
	case "nopasswd":
		return "sudo -n -- " + wrapped, stdin, nil
	case "password":
		if host.SudoPassword == "" {
			return "", nil, fmt.Errorf("sudo password is unavailable")
		}
		if strings.ContainsAny(host.SudoPassword, "\x00\r\n") {
			return "", nil, fmt.Errorf("sudo password contains unsupported control characters")
		}
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		// Frame the command input with a random marker. When sudo has a cached
		// credential (or otherwise does not prompt), it leaves the password unread
		// and would otherwise pass it to the elevated command. The gate below
		// discards everything through the marker before starting the real command,
		// regardless of whether sudo consumed the password line. For `bash -s`,
		// this prevents the password from becoming the first script command.
		marker, err := newSudoInputMarker(host.SudoPassword)
		if err != nil {
			return "", nil, err
		}
		const inputVariable = "OPSNERVA_SUDO_INPUT"
		gate := "set +x; while IFS= read -r " + inputVariable + "; do " +
			"if [ \"$" + inputVariable + "\" = " + shellQuote(marker) + " ]; then " +
			"unset " + inputVariable + "; exec " + wrapped + "; fi; done; exit 1"
		framedInput := io.MultiReader(strings.NewReader(host.SudoPassword+"\n"+marker+"\n"), stdin)
		return "sudo -S -p '' -- bash -c " + shellQuote(gate), framedInput, nil
	default:
		return "", nil, fmt.Errorf("managed sudo is disabled for this host")
	}
}

func newSudoInputMarker(password string) (string, error) {
	var entropy [16]byte
	for {
		if _, err := rand.Read(entropy[:]); err != nil {
			return "", fmt.Errorf("generate managed sudo input marker: %w", err)
		}
		marker := "__OPSNERVA_SUDO_INPUT_" + hex.EncodeToString(entropy[:]) + "__"
		if marker != password {
			return marker, nil
		}
	}
}

func remotePrefix(cwd string, env map[string]string) (string, error) {
	var parts []string
	if cwd != "" {
		if !posixpath.IsAbs(cwd) {
			return "", fmt.Errorf("cwd must be an absolute path")
		}
		parts = append(parts, "cd -- "+shellQuote(cwd)+" &&")
	}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for key := range env {
			if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(key) {
				return "", fmt.Errorf("invalid environment variable name %q", key)
			}
			keys = append(keys, key)
		}
		sortStrings(keys)
		values := make([]string, 0, len(keys)+1)
		values = append(values, "env")
		for _, key := range keys {
			values = append(values, key+"="+shellQuote(env[key]))
		}
		parts = append(parts, strings.Join(values, " "))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, " ") + " ", nil
}

func validateHost(host domain.Host) error {
	if host.Port <= 0 || host.Port > 65535 {
		return fmt.Errorf("invalid SSH port")
	}
	safeTarget := regexp.MustCompile(`^[A-Za-z0-9_.:@%+\[\]-]+$`)
	if host.Address == "" || !safeTarget.MatchString(host.Address) || strings.HasPrefix(host.Address, "-") {
		return fmt.Errorf("invalid SSH address")
	}
	if host.User == "" || !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(host.User) {
		return fmt.Errorf("invalid SSH user")
	}
	return nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

type captureBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type callbackWriter struct {
	stream   string
	callback func(string, []byte)
}

func (w callbackWriter) Write(data []byte) (int, error) {
	w.callback(w.stream, bytes.Clone(data))
	return len(data), nil
}

func newCaptureBuffer() *captureBuffer { return &captureBuffer{} }

func (b *captureBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *captureBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
