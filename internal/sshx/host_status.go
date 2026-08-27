package sshx

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HostStatus is one lightweight sample collected over an active SSH
// connection. CPU and network counters are cumulative so consumers can derive
// current utilization and throughput from consecutive samples.
type HostStatus struct {
	CPUTotal             uint64    `json:"cpu_total"`
	CPUIdle              uint64    `json:"cpu_idle"`
	MemoryUsedBytes      uint64    `json:"memory_used_bytes"`
	MemoryTotalBytes     uint64    `json:"memory_total_bytes"`
	DiskUsedBytes        uint64    `json:"disk_used_bytes"`
	DiskTotalBytes       uint64    `json:"disk_total_bytes"`
	NetworkReceivedBytes uint64    `json:"network_received_bytes"`
	NetworkSentBytes     uint64    `json:"network_sent_bytes"`
	UptimeSeconds        uint64    `json:"uptime_seconds"`
	SampledAt            time.Time `json:"sampled_at"`
}

// HostStatusSession is implemented by interactive SSH sessions that can open
// a separate channel on the same connection for read-only host monitoring.
type HostStatusSession interface {
	HostStatus(context.Context) (HostStatus, error)
}

const hostStatusScript = `LC_ALL=C
awk '/^cpu / { total=0; for (i=2; i<=9; i++) total+=$i; printf "cpu\t%.0f\t%.0f\n", total, $5+$6; exit }' /proc/stat
awk '
  /^MemTotal:/ { total=$2 }
  /^MemFree:/ { free_memory=$2 }
  /^MemAvailable:/ { available=$2 }
  /^Buffers:/ { buffers=$2 }
  /^Cached:/ { cached=$2 }
  END {
    if (available<=0) available=free_memory+buffers+cached
    used=total-available
    if (used<0) used=0
    printf "memory\t%.0f\t%.0f\n", used*1024, total*1024
  }
' /proc/meminfo
df -Pk / | awk 'NR==2 { printf "disk\t%.0f\t%.0f\n", $3*1024, $2*1024 }'
awk '$0 ~ /:/ { split($1, name, ":"); if (name[1]!="lo") { received+=$2; sent+=$10 } } END { printf "network\t%.0f\t%.0f\n", received, sent }' /proc/net/dev
awk '{ printf "uptime\t%.0f\n", $1; exit }' /proc/uptime
`

func (s *nativeShellSession) HostStatus(ctx context.Context) (HostStatus, error) {
	select {
	case <-s.done:
		return HostStatus{}, fmt.Errorf("SSH shell is closed")
	default:
	}
	session, err := s.client.client.NewSession()
	if err != nil {
		return HostStatus{}, fmt.Errorf("create SSH monitoring channel: %w", err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdin = strings.NewReader(hostStatusScript)
	session.Stdout = &stdout
	session.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(remoteCommandShell + " -s") }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = session.Close()
		return HostStatus{}, ctx.Err()
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return HostStatus{}, fmt.Errorf("collect SSH host status: %s", message)
		}
		return HostStatus{}, fmt.Errorf("collect SSH host status: %w", err)
	}
	status, err := parseHostStatus(stdout.String())
	if err != nil {
		return HostStatus{}, err
	}
	status.SampledAt = time.Now().UTC()
	return status, nil
}

func parseHostStatus(output string) (HostStatus, error) {
	var status HostStatus
	seen := make(map[string]bool, 5)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		values := make([]uint64, 0, len(fields)-1)
		for _, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return HostStatus{}, fmt.Errorf("parse SSH host status %q: %w", fields[0], err)
			}
			values = append(values, value)
		}
		switch fields[0] {
		case "cpu":
			if len(values) == 2 {
				status.CPUTotal, status.CPUIdle = values[0], values[1]
				seen[fields[0]] = true
			}
		case "memory":
			if len(values) == 2 {
				status.MemoryUsedBytes, status.MemoryTotalBytes = values[0], values[1]
				seen[fields[0]] = true
			}
		case "disk":
			if len(values) == 2 {
				status.DiskUsedBytes, status.DiskTotalBytes = values[0], values[1]
				seen[fields[0]] = true
			}
		case "network":
			if len(values) == 2 {
				status.NetworkReceivedBytes, status.NetworkSentBytes = values[0], values[1]
				seen[fields[0]] = true
			}
		case "uptime":
			if len(values) == 1 {
				status.UptimeSeconds = values[0]
				seen[fields[0]] = true
			}
		}
	}
	for _, field := range []string{"cpu", "memory", "disk", "network", "uptime"} {
		if !seen[field] {
			return HostStatus{}, fmt.Errorf("SSH host status is missing %s data", field)
		}
	}
	if status.CPUTotal == 0 || status.CPUIdle > status.CPUTotal || status.MemoryTotalBytes == 0 || status.MemoryUsedBytes > status.MemoryTotalBytes || status.DiskTotalBytes == 0 || status.DiskUsedBytes > status.DiskTotalBytes {
		return HostStatus{}, fmt.Errorf("SSH host returned invalid status counters")
	}
	return status, nil
}
