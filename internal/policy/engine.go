package policy

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	posixpath "path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"eino-ops-agent/internal/domain"

	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

type Rule struct {
	Name     string   `yaml:"name"`
	Action   string   `yaml:"action"`
	Programs []string `yaml:"programs"`
	Contains []string `yaml:"contains"`
	Hosts    []string `yaml:"hosts"`
	Paths    []string `yaml:"paths"`
}

type File struct {
	Rules []Rule `yaml:"rules"`
}

type Engine struct {
	rules []Rule
}

var safePrograms = stringSet(
	"arch", "basename", "cat", "cut", "date", "df", "dirname", "dmesg", "du", "env", "find",
	"free", "getent", "grep", "head", "hostname", "id", "ip", "journalctl", "last", "ls", "lscpu",
	"lsblk", "lsof", "netstat", "pgrep", "printenv", "ps", "pwd", "ss", "stat", "tail", "top",
	"uname", "uptime", "vmstat", "wc", "who", "whoami",
	"cmp", "printf", "set", "sha256sum", "sync", "test", "timeout",
)

// These otherwise read-only programs can disclose arbitrary file content.
// They share the same approval boundary as the structured file-read tools so
// an Agent cannot bypass a reviewed read by switching to ssh_exec.
var fileContentPrograms = stringSet("cat", "cut", "grep", "head", "less", "more", "tail")

var changePrograms = stringSet(
	"apt", "apt-get", "apk", "brew", "cargo", "chgrp", "chmod", "chown", "cp", "dnf", "docker",
	"dpkg", "git", "go", "helm", "install", "kill", "kubectl", "make", "mkdir", "mv", "npm", "pip",
	"pip3", "pkill", "podman", "python", "python3", "restorecon", "rm", "rmdir", "rsync", "scp", "sed",
	"service", "sftp", "snap", "systemctl", "tar", "tee", "touch", "unzip", "yarn", "yum", "patch",
)

var criticalPrograms = stringSet(
	"badblocks", "blkdiscard", "cfdisk", "chpasswd", "cryptsetup", "dd", "fdisk", "halt", "iptables",
	"mkfs", "mkfs.btrfs", "mkfs.ext4", "mkfs.xfs", "mkswap", "nft", "parted", "passwd", "poweroff",
	"reboot", "shutdown", "sudo", "swapoff", "ufw", "userdel", "visudo", "wipefs",
)

var forbiddenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:^|[[:space:]])(?:cat|head|tail|less|more|cp|scp|sftp|curl)[^\n]*(?:\.ssh/(?:id_|authorized_keys)|/etc/shadow|master\.key)`),
	regexp.MustCompile(`(?i)OPS_AGENT_MASTER_KEY`),
	regexp.MustCompile(`(?i)(?:disable|stop|kill)[^\n]*(?:ops-agent|audit)`),
}

func Load(path string) (*Engine, error) {
	engine := &Engine{}
	if path == "" {
		return engine, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return engine, nil
		}
		return nil, err
	}
	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse policy file: %w", err)
	}
	for i, rule := range file.Rules {
		if rule.Name == "" {
			return nil, fmt.Errorf("policy rule %d has no name", i)
		}
		switch rule.Action {
		case "allow", "approval", "critical", "deny":
		default:
			return nil, fmt.Errorf("policy rule %q has invalid action %q", rule.Name, rule.Action)
		}
	}
	engine.rules = file.Rules
	return engine, nil
}

func (e *Engine) Evaluate(_ context.Context, host domain.Host, req domain.ExecRequest) domain.Decision {
	source, err := shellSource(req)
	if err != nil {
		return decision(domain.RiskCritical, domain.ActionBreakGlass, err.Error(), "invalid_request")
	}
	for _, pattern := range forbiddenPatterns {
		if pattern.MatchString(source) {
			return decision(domain.RiskForbidden, domain.ActionDeny, "operation attempts to access credentials or disable controls", "builtin_forbidden")
		}
	}

	risk, hits, parseErr := analyzeShell(source)
	if parseErr != nil {
		return decision(domain.RiskCritical, domain.ActionBreakGlass, "shell syntax cannot be safely analyzed", "shell_parse_failed")
	}
	if req.Elevated {
		risk = maxRisk(risk, domain.RiskCritical)
		hits = append(hits, "managed_sudo")
	}

	for _, rule := range e.rules {
		if !ruleMatches(rule, host, source, req) {
			continue
		}
		hits = append(hits, "yaml:"+rule.Name)
		switch rule.Action {
		case "deny":
			return domain.Decision{Risk: domain.RiskForbidden, Action: domain.ActionDeny, Reason: "denied by policy rule " + rule.Name, RuleHits: unique(hits)}
		case "critical":
			risk = maxRisk(risk, domain.RiskCritical)
		case "approval":
			risk = maxRisk(risk, domain.RiskChange)
		case "allow":
			if risk == domain.RiskChange {
				risk = domain.RiskReadOnly
			}
		}
	}
	if req.Mode == domain.ExecWorkspaceShell {
		// Arbitrary local scripts always require an exact human approval. Static
		// command classification cannot prove that wrapper programs such as env,
		// find, or an interpreter invocation are free of workspace mutations.
		risk = maxRisk(risk, domain.RiskChange)
		if req.WorkspaceShellBackend == domain.WorkspaceShellModeHost {
			hits = append(hits, "workspace_host_shell")
		} else {
			hits = append(hits, "workspace_sandbox_shell")
		}
	}
	if req.Mode == domain.ExecWorkspaceEdit {
		risk = maxRisk(risk, domain.RiskChange)
		hits = append(hits, "workspace_file_edit")
	}
	if req.Mode == domain.ExecRemoteEdit {
		risk = maxRisk(risk, domain.RiskChange)
		hits = append(hits, "ssh_file_edit")
	}
	if req.Mode == domain.ExecSSHFileTransfer {
		risk = maxRisk(risk, domain.RiskChange)
		hits = append(hits, "ssh_host_file_transfer")
	}
	if req.Mode == domain.ExecSSHTunnelStart {
		risk = maxRisk(risk, domain.RiskChange)
		hits = append(hits, "ssh_local_port_forward")
	}
	fileContentAccess := isFileContentAccess(req.Mode) || hasFileContentProgram(hits)
	if fileContentAccess {
		hits = append(hits, "file_content_access")
	}

	sort.Strings(hits)
	switch risk {
	case domain.RiskReadOnly:
		if fileContentAccess {
			return domain.Decision{Risk: risk, Action: domain.ActionApprove, Reason: "reading file content requires human approval", RuleHits: unique(hits)}
		}
		return domain.Decision{Risk: risk, Action: domain.ActionAllow, Reason: "read-only command", RuleHits: unique(hits)}
	case domain.RiskChange:
		return domain.Decision{Risk: risk, Action: domain.ActionApprove, Reason: "operation may change remote state", RuleHits: unique(hits)}
	case domain.RiskCritical:
		return domain.Decision{Risk: risk, Action: domain.ActionBreakGlass, Reason: "privileged or destructive operation", RuleHits: unique(hits)}
	default:
		return domain.Decision{Risk: domain.RiskForbidden, Action: domain.ActionDeny, Reason: "operation is forbidden", RuleHits: unique(hits)}
	}
}

func isFileContentAccess(mode domain.ExecMode) bool {
	switch mode {
	case domain.ExecRemoteRead, domain.ExecRemoteSearch, domain.ExecWorkspaceRead, domain.ExecWorkspaceSearch:
		return true
	default:
		return false
	}
}

func hasFileContentProgram(hits []string) bool {
	for _, hit := range hits {
		program, ok := strings.CutPrefix(hit, "read_program:")
		if ok && fileContentPrograms[program] {
			return true
		}
	}
	return false
}

func analyzeShell(source string) (domain.RiskLevel, []string, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(source), "remote.sh")
	if err != nil {
		return domain.RiskCritical, nil, err
	}
	risk := domain.RiskReadOnly
	var hits []string
	commandCount := 0
	syntax.Walk(file, func(node syntax.Node) bool {
		switch typed := node.(type) {
		case *syntax.CallExpr:
			if len(typed.Args) == 0 {
				return true
			}
			program := baseProgram(printNode(typed.Args[0]))
			commandCount++
			switch {
			case criticalPrograms[program]:
				risk = maxRisk(risk, domain.RiskCritical)
				hits = append(hits, "critical_program:"+program)
			case changePrograms[program]:
				if program == "rm" {
					risk = maxRisk(risk, domain.RiskCritical)
					hits = append(hits, "critical_program:rm")
				} else {
					risk = maxRisk(risk, domain.RiskChange)
					hits = append(hits, "change_program:"+program)
				}
			case safePrograms[program]:
				hits = append(hits, "read_program:"+program)
			default:
				risk = maxRisk(risk, domain.RiskChange)
				hits = append(hits, "unknown_program:"+program)
			}
		case *syntax.Redirect:
			risk = maxRisk(risk, domain.RiskChange)
			hits = append(hits, "shell_redirection")
		case *syntax.CmdSubst, *syntax.ProcSubst:
			risk = maxRisk(risk, domain.RiskCritical)
			hits = append(hits, "dynamic_shell_expansion")
		}
		return true
	})
	if commandCount == 0 {
		return domain.RiskCritical, hits, fmt.Errorf("no executable command found")
	}
	lower := strings.ToLower(source)
	if regexp.MustCompile(`(?:curl|wget)[^\n|]*\|\s*(?:ba)?sh`).MatchString(lower) {
		risk = domain.RiskCritical
		hits = append(hits, "download_pipe_shell")
	}
	if regexp.MustCompile(`\b(?:eval|source)\b`).MatchString(lower) {
		risk = domain.RiskCritical
		hits = append(hits, "dynamic_code_execution")
	}
	return risk, unique(hits), nil
}

func ContainsProgram(req domain.ExecRequest, name string) (bool, error) {
	source, err := shellSource(req)
	if err != nil {
		return false, err
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(source), "remote.sh")
	if err != nil {
		return false, err
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if baseProgram(printNode(call.Args[0])) == name {
			found = true
			return false
		}
		return true
	})
	return found, nil
}

func shellSource(req domain.ExecRequest) (string, error) {
	if req.Mode == "" {
		if req.Script != "" {
			req.Mode = domain.ExecScript
		} else {
			req.Mode = domain.ExecProgram
		}
	}
	switch req.Mode {
	case domain.ExecProgram:
		if !regexp.MustCompile(`^[A-Za-z0-9_./+:-]+$`).MatchString(req.Program) {
			return "", fmt.Errorf("program contains unsupported characters")
		}
		parts := []string{shellQuote(req.Program)}
		for _, arg := range req.Args {
			parts = append(parts, shellQuote(arg))
		}
		return strings.Join(parts, " "), nil
	case domain.ExecScript:
		if strings.TrimSpace(req.Script) == "" {
			return "", fmt.Errorf("script is empty")
		}
		return req.Script, nil
	case domain.ExecWorkspaceShell:
		if req.WorkspaceID == "" || strings.TrimSpace(req.Script) == "" {
			return "", fmt.Errorf("workspace shell requires a workspace and script")
		}
		return req.Script, nil
	case domain.ExecWorkspaceRead:
		return "cat " + shellQuote(req.WorkspaceID+"/"+req.RelativePath), nil
	case domain.ExecWorkspaceDirectoryList:
		return "ls " + shellQuote(req.WorkspaceID+"/"+req.RelativePath), nil
	case domain.ExecWorkspaceSearch:
		flag, err := fileSearchFlag(req.SearchMatchMode)
		if err != nil {
			return "", err
		}
		return "grep " + flag + " " + shellQuote(req.SearchPattern) + " " + shellQuote(req.WorkspaceID+"/"+req.RelativePath), nil
	case domain.ExecRemoteRead:
		if !posixpath.IsAbs(req.RemotePath) {
			return "", fmt.Errorf("remote file read requires an absolute path")
		}
		return "cat " + shellQuote(req.RemotePath), nil
	case domain.ExecRemoteSearch:
		if !posixpath.IsAbs(req.RemotePath) || req.SearchPattern == "" {
			return "", fmt.Errorf("remote file search requires an absolute path and pattern")
		}
		flag, err := fileSearchFlag(req.SearchMatchMode)
		if err != nil {
			return "", err
		}
		return "grep " + flag + " " + shellQuote(req.SearchPattern) + " " + shellQuote(req.RemotePath), nil
	case domain.ExecWorkspaceEdit:
		if req.WorkspaceID == "" || req.RelativePath == "" || req.Change == nil || req.Change.Diff == "" {
			return "", fmt.Errorf("workspace edit requires a workspace file and a structured change")
		}
		return "edit " + shellQuote(req.WorkspaceID+"/"+req.RelativePath), nil
	case domain.ExecRemoteEdit:
		if req.HostID == "" || !posixpath.IsAbs(req.RemotePath) || req.Change == nil || req.Change.Diff == "" {
			return "", fmt.Errorf("remote edit requires a host, absolute file path, and structured change")
		}
		return "edit " + shellQuote(req.RemotePath), nil
	case domain.ExecWorkspaceUpload:
		if req.WorkspaceID == "" || req.RelativePath == "" || req.ExpectedSHA256 == "" || !posixpath.IsAbs(req.RemotePath) {
			return "", fmt.Errorf("workspace upload requires a workspace file, expected SHA256, and absolute remote path")
		}
		return "sftp put " + shellQuote(req.WorkspaceID+"/"+req.RelativePath) + " " + shellQuote(req.RemotePath), nil
	case domain.ExecSSHFileTransfer:
		if req.SourceHostID == "" || req.SourcePath == "" || req.HostID == "" || req.RemotePath == "" || req.ExpectedSHA256 == "" {
			return "", fmt.Errorf("SSH file transfer requires source and destination hosts, paths, and source SHA256")
		}
		return "sftp get " + shellQuote(req.SourceHostID+":"+req.SourcePath) + " && sftp put " + shellQuote(req.HostID+":"+req.RemotePath), nil
	case domain.ExecSSHTunnelStart:
		if req.HostID == "" || req.TunnelRemoteHost == "" || req.TunnelRemotePort < 1 {
			return "", fmt.Errorf("SSH tunnel requires a host and remote endpoint")
		}
		return "ssh-tunnel --local " + shellQuote(fmt.Sprintf("127.0.0.1:%d", req.TunnelLocalPort)) +
			" --remote " + shellQuote(net.JoinHostPort(req.TunnelRemoteHost, strconv.Itoa(req.TunnelRemotePort))), nil
	default:
		return "", fmt.Errorf("unsupported execution mode %q", req.Mode)
	}
}

func ruleMatches(rule Rule, host domain.Host, source string, req domain.ExecRequest) bool {
	if len(rule.Hosts) > 0 && !contains(rule.Hosts, host.ID) && !contains(rule.Hosts, host.Name) && !contains(rule.Hosts, "*") {
		return false
	}
	if len(rule.Programs) > 0 {
		program := baseProgram(req.Program)
		matched := false
		for _, candidate := range rule.Programs {
			if candidate == "*" || candidate == program {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(rule.Contains) > 0 {
		matched := false
		for _, fragment := range rule.Contains {
			if strings.Contains(source, fragment) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(rule.Paths) > 0 {
		matched := false
		for _, path := range rule.Paths {
			if strings.HasPrefix(posixpath.Clean(req.Cwd), posixpath.Clean(path)) || strings.Contains(source, path) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func decision(risk domain.RiskLevel, action domain.DecisionAction, reason, hit string) domain.Decision {
	return domain.Decision{Risk: risk, Action: action, Reason: reason, RuleHits: []string{hit}}
}

func maxRisk(left, right domain.RiskLevel) domain.RiskLevel {
	rank := map[domain.RiskLevel]int{domain.RiskReadOnly: 0, domain.RiskChange: 1, domain.RiskCritical: 2, domain.RiskForbidden: 3}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func baseProgram(value string) string {
	value = strings.Trim(value, "'\"")
	return posixpath.Base(value)
}

func printNode(node syntax.Node) string {
	var buf bytes.Buffer
	_ = syntax.NewPrinter().Print(&buf, node)
	return buf.String()
}

func fileSearchFlag(matchMode domain.FileSearchMatchMode) (string, error) {
	switch matchMode {
	case domain.FileSearchLiteral:
		return "-F", nil
	case domain.FileSearchRegex:
		return "-E", nil
	default:
		return "", fmt.Errorf("file search match_mode must be literal or regex")
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func stringSet(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
