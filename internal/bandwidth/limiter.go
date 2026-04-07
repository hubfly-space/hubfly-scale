package bandwidth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"hubfly-scale/internal/docker"
)

type Limiter struct {
	docker docker.Client
	logger *log.Logger
}

type UpdateRequest struct {
	EgressMbps  float64 `json:"egress_mbps"`
	IngressMbps float64 `json:"ingress_mbps"`
}

func NewLimiter(dc docker.Client, logger *log.Logger) *Limiter {
	return &Limiter{docker: dc, logger: logger}
}

func (l *Limiter) Apply(ctx context.Context, containerName string, req UpdateRequest) error {
	if req.EgressMbps <= 0 && req.IngressMbps <= 0 {
		return fmt.Errorf("egress_mbps or ingress_mbps must be > 0")
	}
	pid, err := l.docker.InspectPID(ctx, containerName)
	if err != nil {
		return fmt.Errorf("inspect pid: %w", err)
	}
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	netns, err := l.docker.InspectSandboxKey(ctx, containerName)
	if err != nil {
		l.logger.Printf("container=%s inspect sandbox key failed: %v", containerName, err)
	}
	ifindex, err := containerIfindex(ctx, pid, netns)
	if err != nil {
		return err
	}
	veth, err := hostVethByPeerIndex(ifindex)
	if err != nil {
		iflink, err2 := containerPeerIfindex(ctx, pid, netns)
		if err2 == nil {
			veth, err = hostVethName(iflink)
		}
		if err != nil {
			return err
		}
	}
	l.logger.Printf("container=%s iface_ifindex=%d host_veth=%s", containerName, ifindex, veth)

	_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", veth, "root")
	_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", veth, "ingress")

	if req.EgressMbps > 0 {
		if err := applyEgressLimit(ctx, veth, req.EgressMbps); err != nil {
			return err
		}
	}
	if req.IngressMbps > 0 {
		if err := applyIngressLimit(ctx, veth, ifindex, req.IngressMbps); err != nil {
			return err
		}
	}
	l.logger.Printf("container=%s bandwidth updated egress=%.2f ingress=%.2f", containerName, req.EgressMbps, req.IngressMbps)
	return nil
}

func containerIfindex(ctx context.Context, pid int, netns string) (int, error) {
	iface, err := containerIfaceName(ctx, pid, netns)
	if err != nil {
		return 0, err
	}
	line, err := runNetns(ctx, pid, netns, "ip", "-o", "link", "show", iface)
	if err != nil {
		return 0, fmt.Errorf("read iface index (%s): %w", iface, err)
	}
	idx, ok := parseIfaceIndex(line)
	if !ok {
		return 0, fmt.Errorf("parse iface index (%s)", iface)
	}
	return idx, nil
}

func containerPeerIfindex(ctx context.Context, pid int, netns string) (int, error) {
	iface, err := containerIfaceName(ctx, pid, netns)
	if err != nil {
		return 0, err
	}
	line, err := runNetns(ctx, pid, netns, "ip", "-o", "link", "show", iface)
	if err == nil {
		if peer, ok := parsePeerIfindex(line); ok {
			return peer, nil
		}
	}

	// Fallback to sysfs iflink.
	path := "/sys/class/net/" + iface + "/iflink"
	out, err := runNetns(ctx, pid, netns, "cat", path)
	if err != nil {
		return 0, fmt.Errorf("read iflink (%s): %w", iface, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return 0, fmt.Errorf("empty iflink for %s", iface)
	}
	iflink, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("parse iflink (%s): %w", iface, err)
	}
	return iflink, nil
}

func containerIfaceName(ctx context.Context, pid int, netns string) (string, error) {
	// Prefer eth0 if present (script-style).
	if out, err := runNetns(ctx, pid, netns, "ip", "-o", "link", "show", "eth0"); err == nil {
		if _, ok := parseIfaceIndex(out); ok {
			return "eth0", nil
		}
	}
	out, err := runNetns(ctx, pid, netns, "ls", "/sys/class/net")
	if err != nil {
		return "", fmt.Errorf("list netns interfaces: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("no interfaces in netns")
	}
	for _, name := range fields {
		if name != "lo" {
			return name, nil
		}
	}
	return "", fmt.Errorf("no non-loopback interface found")
}

func parsePeerIfindex(line string) (int, bool) {
	// Example: "2: eth0@if24: <...>"
	re := regexp.MustCompile(`@if(\d+)`)
	m := re.FindStringSubmatch(line)
	if len(m) != 2 {
		return 0, false
	}
	val, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return val, true
}

func parseIfaceIndex(line string) (int, bool) {
	colon := strings.Index(line, ":")
	if colon <= 0 {
		return 0, false
	}
	prefix := strings.TrimSpace(line[:colon])
	val, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, false
	}
	return val, true
}

func hostVethName(iflink int) (string, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return "", fmt.Errorf("read /sys/class/net: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		ifindexPath := filepath.Join("/sys/class/net", name, "ifindex")
		data, err := os.ReadFile(ifindexPath)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				continue
			}
			continue
		}
		idxStr := strings.TrimSpace(string(data))
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		if idx == iflink {
			return name, nil
		}
	}
	return "", fmt.Errorf("host veth not found for ifindex %d", iflink)
}

func hostVethByPeerIndex(peerIndex int) (string, error) {
	out, err := run(context.Background(), "ip", "-o", "link")
	if err != nil {
		return "", fmt.Errorf("list host links: %w", err)
	}
	lines := strings.Split(out, "\n")
	needle := fmt.Sprintf("@if%d:", peerIndex)
	for _, line := range lines {
		if strings.Contains(line, needle) {
			parts := strings.SplitN(line, ": ", 2)
			if len(parts) < 2 {
				continue
			}
			name := parts[1]
			if idx := strings.Index(name, "@"); idx >= 0 {
				name = name[:idx]
			}
			name = strings.TrimSpace(name)
			if name != "" {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("host veth not found for peer ifindex %d", peerIndex)
}

func applyEgressLimit(ctx context.Context, dev string, mbps float64) error {
	rate := formatRate(mbps)
	return applyTBF(ctx, dev, rate)
}

func applyIngressLimit(ctx context.Context, dev string, iflink int, mbps float64) error {
	rate := formatRate(mbps)
	ifbName := fmt.Sprintf("ifb%d", iflink)

	if err := ensureIFB(ctx, ifbName); err != nil {
		return err
	}
	_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", ifbName, "root")
	if err := runQuiet(ctx, "tc", "qdisc", "add", "dev", dev, "handle", "ffff:", "ingress"); err != nil {
		return fmt.Errorf("tc ingress qdisc: %w", err)
	}
	if err := runQuiet(ctx, "tc", "filter", "add", "dev", dev, "parent", "ffff:", "protocol", "ip", "u32", "match", "u32", "0", "0", "action", "mirred", "egress", "redirect", "dev", ifbName); err != nil {
		return fmt.Errorf("tc ingress filter: %w", err)
	}
	return applyTBF(ctx, ifbName, rate)
}

func applyTBF(ctx context.Context, dev, rate string) error {
	if err := runQuiet(ctx, "tc", "qdisc", "add", "dev", dev, "root", "tbf", "rate", rate, "burst", "100kbit", "latency", "100ms"); err != nil {
		return fmt.Errorf("tc tbf qdisc: %w", err)
	}
	return nil
}

func ensureIFB(ctx context.Context, ifbName string) error {
	if err := runQuiet(ctx, "ip", "link", "show", ifbName); err == nil {
		return runQuiet(ctx, "ip", "link", "set", ifbName, "up")
	}
	if err := runQuiet(ctx, "ip", "link", "add", ifbName, "type", "ifb"); err != nil {
		return fmt.Errorf("create ifb: %w", err)
	}
	return runQuiet(ctx, "ip", "link", "set", ifbName, "up")
}

func formatRate(mbps float64) string {
	if mbps <= 0 {
		return "1kbit"
	}
	kbit := int(math.Round(mbps * 1000))
	if kbit < 1 {
		kbit = 1
	}
	return fmt.Sprintf("%dkbit", kbit)
}

// Burst is fixed to match the recommended script behavior.

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runQuiet(ctx context.Context, name string, args ...string) error {
	_, err := run(ctx, name, args...)
	return err
}

func runNetns(ctx context.Context, pid int, netns string, args ...string) (string, error) {
	cmd := []string{"nsenter"}
	if netns != "" && netns != "<no value>" {
		cmd = append(cmd, "--net="+netns)
	} else {
		cmd = append(cmd, "-t", strconv.Itoa(pid), "-n")
	}
	cmd = append(cmd, args...)
	return run(ctx, cmd[0], cmd[1:]...)
}

// no custom error helpers
