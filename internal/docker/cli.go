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
	"strings"
	"time"
)

type Client interface {
	InspectIP(ctx context.Context, containerName string) (string, error)
	InspectPaused(ctx context.Context, containerName string) (bool, error)
	Pause(ctx context.Context, containerName string) error
	Unpause(ctx context.Context, containerName string) error
	Stats(ctx context.Context, containerName string) (ContainerStats, error)
	UpdateCPU(ctx context.Context, containerName string, nanoCPUs int64) error
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

func (c *CLIClient) InspectIP(ctx context.Context, containerName string) (string, error) {
	out, err := runDocker(ctx, "inspect", "-f", "{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}", containerName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (c *CLIClient) InspectPaused(ctx context.Context, containerName string) (bool, error) {
	out, err := runDocker(ctx, "inspect", "-f", "{{.State.Paused}}", containerName)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
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

	var decoded statsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return ContainerStats{}, fmt.Errorf("decode stats: %w", err)
	}
	online := decoded.CPUStats.OnlineCPUs
	if online == 0 {
		online = uint64(len(decoded.CPUStats.CPUUsage.PerCPUUsage))
	}
	return ContainerStats{
		TotalUsage:  decoded.CPUStats.CPUUsage.TotalUsage,
		SystemUsage: decoded.CPUStats.SystemUsage,
		OnlineCPUs:  online,
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
