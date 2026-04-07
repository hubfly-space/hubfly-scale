package bandwidth

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"hubfly-scale/internal/docker"
)

type NetworkLimiter struct {
	docker docker.Client
	logger *log.Logger
}

func NewNetworkLimiter(dc docker.Client, logger *log.Logger) *NetworkLimiter {
	return &NetworkLimiter{docker: dc, logger: logger}
}

func (l *NetworkLimiter) Apply(ctx context.Context, networkName string, req UpdateRequest) error {
	if req.EgressMbps <= 0 && req.IngressMbps <= 0 {
		return fmt.Errorf("egress_mbps or ingress_mbps must be > 0")
	}
	bridge, err := l.docker.InspectNetworkBridge(ctx, networkName)
	if err != nil {
		return fmt.Errorf("inspect network bridge: %w", err)
	}
	if bridge == "" {
		return fmt.Errorf("bridge not found for network %s", networkName)
	}
	if _, err := os.Stat(filepath.Join("/sys/class/net", bridge)); err != nil {
		return fmt.Errorf("bridge interface %s not found", bridge)
	}

	ifindex, err := readIfindex(bridge)
	if err != nil {
		return err
	}
	ifbName := fmt.Sprintf("ifb%d", ifindex)
	l.logger.Printf("network=%s bridge=%s ifb=%s", networkName, bridge, ifbName)

	_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", bridge, "root")
	_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", bridge, "ingress")
	_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", bridge, "clsact")
	_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", ifbName, "root")

	rate := req.EgressMbps
	if rate <= 0 {
		rate = req.IngressMbps
	}
	if err := applySharedCap(ctx, bridge, rate); err != nil {
		return fmt.Errorf("network cap: %w", err)
	}

	if req.IngressMbps > 0 {
		if err := ensureIFB(ctx, ifbName); err != nil {
			return err
		}
		if err := applyIngressRedirectOnPorts(ctx, bridge, ifbName); err != nil {
			return err
		}
		if err := applySharedCap(ctx, ifbName, req.IngressMbps); err != nil {
			return fmt.Errorf("ingress cap: %w", err)
		}
	}

	l.logger.Printf("network=%s bandwidth updated egress=%.2f ingress=%.2f", networkName, req.EgressMbps, req.IngressMbps)
	return nil
}

func applySharedCap(ctx context.Context, dev string, mbps float64) error {
	rate := formatRate(mbps)
	if err := runQuiet(ctx, "tc", "qdisc", "add", "dev", dev, "root", "handle", "1:", "htb", "default", "1"); err != nil {
		return err
	}
	if err := runQuiet(ctx, "tc", "class", "add", "dev", dev, "parent", "1:", "classid", "1:1", "htb", "rate", rate, "ceil", rate); err != nil {
		return err
	}
	if err := runQuiet(ctx, "tc", "qdisc", "add", "dev", dev, "parent", "1:1", "handle", "10:", "fq_codel"); err != nil {
		// Fallback to sfq if fq_codel is unavailable.
		if !strings.Contains(err.Error(), "No such file or directory") {
			return err
		}
		if err := runQuiet(ctx, "tc", "qdisc", "add", "dev", dev, "parent", "1:1", "handle", "10:", "sfq"); err != nil {
			return err
		}
	}
	return nil
}

func applyIngressRedirectOnPorts(ctx context.Context, bridge, ifb string) error {
	ports, err := listBridgePorts(ctx, bridge)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return fmt.Errorf("no bridge ports found for %s", bridge)
	}
	for _, port := range ports {
		_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", port, "ingress")
		_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", port, "clsact")
		if err := runQuiet(ctx, "tc", "qdisc", "add", "dev", port, "clsact"); err == nil {
			if err := runQuiet(ctx, "tc", "filter", "add", "dev", port, "ingress", "matchall", "action", "mirred", "egress", "redirect", "dev", ifb); err == nil {
				continue
			}
		}
		_ = runQuiet(ctx, "tc", "qdisc", "del", "dev", port, "clsact")
		if err := runQuiet(ctx, "tc", "qdisc", "add", "dev", port, "handle", "ffff:", "ingress"); err != nil {
			return fmt.Errorf("tc ingress qdisc port %s: %w", port, err)
		}
		if err := runQuiet(ctx, "tc", "filter", "add", "dev", port, "parent", "ffff:", "protocol", "ip", "u32", "match", "u32", "0", "0", "action", "mirred", "egress", "redirect", "dev", ifb); err != nil {
			return fmt.Errorf("tc ingress filter port %s: %w", port, err)
		}
		if err := runQuiet(ctx, "tc", "filter", "add", "dev", port, "parent", "ffff:", "protocol", "ipv6", "u32", "match", "u32", "0", "0", "action", "mirred", "egress", "redirect", "dev", ifb); err != nil {
			return fmt.Errorf("tc ingress filter ipv6 port %s: %w", port, err)
		}
	}
	return nil
}

func listBridgePorts(ctx context.Context, bridge string) ([]string, error) {
	out, err := run(ctx, "ip", "-o", "link", "show", "master", bridge)
	if err != nil {
		return nil, fmt.Errorf("list bridge ports: %w", err)
	}
	var ports []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
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
			ports = append(ports, name)
		}
	}
	return ports, nil
}

func readIfindex(dev string) (int, error) {
	data, err := os.ReadFile(filepath.Join("/sys/class/net", dev, "ifindex"))
	if err != nil {
		return 0, fmt.Errorf("read ifindex: %w", err)
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
