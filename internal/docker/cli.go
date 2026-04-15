package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client interface {
	InspectID(ctx context.Context, containerName string) (string, error)
	InspectIP(ctx context.Context, containerName string) (string, error)
	InspectPaused(ctx context.Context, containerName string) (bool, error)
	InspectPID(ctx context.Context, containerName string) (int, error)
	InspectSandboxKey(ctx context.Context, containerName string) (string, error)
	InspectNetworkBridge(ctx context.Context, networkName string) (string, error)
	Pause(ctx context.Context, containerName string) error
	Unpause(ctx context.Context, containerName string) error
	Stats(ctx context.Context, containerName string) (ContainerStats, error)
	UpdateCPU(ctx context.Context, containerName string, nanoCPUs int64) error
	UpdateMemory(ctx context.Context, containerName string, bytes int64) error
}

type CLIClient struct {
	socketPath string
	apiVersion string
	httpClient *http.Client
}

func NewCLIClient() *CLIClient {
	socketPath := getenv("HF_DOCKER_SOCKET", "/var/run/docker.sock")
	apiVersion := getenv("HF_DOCKER_API_VERSION", "v1.41")
	return &CLIClient{
		socketPath: socketPath,
		apiVersion: apiVersion,
		httpClient: newDockerHTTPClient(socketPath),
	}
}

func (c *CLIClient) InspectID(ctx context.Context, containerName string) (string, error) {
	out, err := runDocker(ctx, "inspect", "-f", "{{.Id}}", containerName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (c *CLIClient) InspectIP(ctx context.Context, containerName string) (string, error) {
	out, err := runDocker(ctx, "inspect", "-f", "{{json .NetworkSettings.Networks}}", containerName)
	if err != nil {
		return "", err
	}
	type networkInfo struct {
		IPAddress string `json:"IPAddress"`
	}
	var networks map[string]networkInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &networks); err != nil {
		return "", fmt.Errorf("decode inspect networks: %w", err)
	}

	ips := make([]string, 0, len(networks))
	for _, network := range networks {
		ip := strings.TrimSpace(network.IPAddress)
		if parsed := net.ParseIP(ip); parsed != nil {
			ips = append(ips, parsed.String())
		}
	}
	if len(ips) == 0 {
		return "", nil
	}
	sort.Strings(ips)
	return ips[0], nil
}

func (c *CLIClient) InspectPaused(ctx context.Context, containerName string) (bool, error) {
	out, err := runDocker(ctx, "inspect", "-f", "{{.State.Paused}}", containerName)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

func (c *CLIClient) InspectPID(ctx context.Context, containerName string) (int, error) {
	out, err := runDocker(ctx, "inspect", "-f", "{{.State.Pid}}", containerName)
	if err != nil {
		return 0, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return 0, fmt.Errorf("empty pid")
	}
	pid, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("parse pid: %w", err)
	}
	return pid, nil
}

func (c *CLIClient) InspectSandboxKey(ctx context.Context, containerName string) (string, error) {
	out, err := runDocker(ctx, "inspect", "-f", "{{.NetworkSettings.SandboxKey}}", containerName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (c *CLIClient) InspectNetworkBridge(ctx context.Context, networkName string) (string, error) {
	out, err := runDocker(ctx, "network", "inspect", "-f", `{{index .Options "com.docker.network.bridge.name"}}`, networkName)
	if err != nil {
		return "", err
	}
	bridge := strings.TrimSpace(out)
	if bridge != "" {
		return bridge, nil
	}
	if networkName == "bridge" {
		return "docker0", nil
	}
	id, err := runDocker(ctx, "network", "inspect", "-f", "{{.Id}}", networkName)
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("empty network id")
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return "br-" + id, nil
}

func (c *CLIClient) Pause(ctx context.Context, containerName string) error {
	_, err := runDocker(ctx, "pause", containerName)
	return err
}

func (c *CLIClient) Unpause(ctx context.Context, containerName string) error {
	_, err := runDocker(ctx, "unpause", containerName)
	return err
}

type ContainerStats struct {
	TotalUsage  uint64
	SystemUsage uint64
	OnlineCPUs  uint64
	MemoryUsage uint64
	MemoryLimit uint64
	MemoryCache uint64
	Failcnt     uint64
	OOMKills    uint64
}

type statsResponse struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PerCPUUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  uint64 `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage   uint64            `json:"usage"`
		Limit   uint64            `json:"limit"`
		Failcnt uint64            `json:"failcnt"`
		Stats   map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

func (c *CLIClient) Stats(ctx context.Context, containerName string) (ContainerStats, error) {
	path := fmt.Sprintf("http://docker/%s/containers/%s/stats?stream=false", c.apiVersion, url.PathEscape(containerName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ContainerStats{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ContainerStats{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return ContainerStats{}, fmt.Errorf("docker stats: %s", strings.TrimSpace(string(body)))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return ContainerStats{}, fmt.Errorf("read stats: %w", err)
	}
	if len(bodyBytes) == 0 {
		return ContainerStats{}, fmt.Errorf("decode stats: empty body")
	}
	var decoded statsResponse
	if err := json.Unmarshal(bodyBytes, &decoded); err != nil {
		return ContainerStats{}, fmt.Errorf("decode stats: %w", err)
	}
	online := decoded.CPUStats.OnlineCPUs
	if online == 0 {
		online = uint64(len(decoded.CPUStats.CPUUsage.PerCPUUsage))
	}
	cache := uint64(0)
	if decoded.MemoryStats.Stats != nil {
		cache = decoded.MemoryStats.Stats["cache"]
		if cache == 0 {
			cache = decoded.MemoryStats.Stats["inactive_file"]
		}
	}
	oomKills := uint64(0)
	if decoded.MemoryStats.Stats != nil {
		oomKills = decoded.MemoryStats.Stats["oom_kill"]
		if oomKills == 0 {
			oomKills = decoded.MemoryStats.Stats["oom_killed"]
		}
	}
	return ContainerStats{
		TotalUsage:  decoded.CPUStats.CPUUsage.TotalUsage,
		SystemUsage: decoded.CPUStats.SystemUsage,
		OnlineCPUs:  online,
		MemoryUsage: decoded.MemoryStats.Usage,
		MemoryLimit: decoded.MemoryStats.Limit,
		MemoryCache: cache,
		Failcnt:     decoded.MemoryStats.Failcnt,
		OOMKills:    oomKills,
	}, nil
}

func (c *CLIClient) UpdateCPU(ctx context.Context, containerName string, nanoCPUs int64) error {
	payload := map[string]any{"NanoCpus": nanoCPUs}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("http://docker/%s/containers/%s/update", c.apiVersion, url.PathEscape(containerName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker update: %s", strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (c *CLIClient) UpdateMemory(ctx context.Context, containerName string, memBytes int64) error {
	if memBytes < 1 {
		memBytes = 1
	}
	payload := map[string]any{"Memory": memBytes}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("http://docker/%s/containers/%s/update", c.apiVersion, url.PathEscape(containerName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker update: %s", strings.TrimSpace(string(respBody)))
	}
	return nil
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func newDockerHTTPClient(socketPath string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
