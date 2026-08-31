package agenttool

import (
	"context"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
)

type TunnelService interface {
	StartSSHTunnel(context.Context, string, domain.SSHTunnelConfig, string, string) (domain.ExecResult, error)
	ListSSHTunnels() domain.SSHTunnelList
	StopSSHTunnel(context.Context, string, string) (domain.SSHTunnel, error)
}

func (ssh *SSH) RunTunnel(ctx context.Context, input SSHTunnelInput, actor string) (any, error) {
	action := strings.ToLower(strings.TrimSpace(input.Action))
	direction := strings.ToLower(strings.TrimSpace(input.Direction))
	if action == "start" && direction != "" && direction != string(domain.SSHTunnelDirectionLocal) && direction != string(domain.SSHTunnelDirectionReverse) {
		return ssh.dependencies.Results.Value(ctx, "ssh_tunnel", domain.SSHTunnel{}, InvalidInput("direction must be local or reverse"))
	}
	switch action {
	case "start":
		if input.TunnelID != "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_tunnel", domain.SSHTunnel{}, InvalidInput("tunnel_id is only valid with action=stop"))
		}
		input.Direction = direction
		result, err := ssh.dependencies.Tunnels.StartSSHTunnel(ctx, input.HostID, domain.SSHTunnelConfig{
			Direction: domain.SSHTunnelDirection(input.Direction), LocalHost: input.LocalHost, LocalPort: input.LocalPort,
			RemoteHost: input.RemoteHost, RemotePort: input.RemotePort,
		}, input.Reason, actor)
		return ssh.execResult(result, err)
	case "list":
		if input.HostID != "" || input.Direction != "" || input.LocalHost != "" || input.LocalPort != 0 || input.RemoteHost != "" || input.RemotePort != 0 || input.TunnelID != "" || input.Reason != "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_tunnel", domain.SSHTunnelList{}, InvalidInput("action=list accepts only action"))
		}
		return ssh.dependencies.Tunnels.ListSSHTunnels(), nil
	case "stop":
		if input.HostID != "" || input.Direction != "" || input.LocalHost != "" || input.LocalPort != 0 || input.RemoteHost != "" || input.RemotePort != 0 || input.Reason != "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_tunnel", domain.SSHTunnel{}, InvalidInput("action=stop accepts only action and tunnel_id"))
		}
		if strings.TrimSpace(input.TunnelID) == "" {
			return ssh.dependencies.Results.Value(ctx, "ssh_tunnel", domain.SSHTunnel{}, InvalidInput("action=stop requires tunnel_id"))
		}
		tunnel, err := ssh.dependencies.Tunnels.StopSSHTunnel(ctx, input.TunnelID, actor)
		return ssh.dependencies.Results.Value(ctx, "ssh_tunnel", tunnel, err)
	default:
		return ssh.dependencies.Results.Value(ctx, "ssh_tunnel", domain.SSHTunnel{}, InvalidInput("invalid action: use start, list, or stop"))
	}
}
