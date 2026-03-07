package docker

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Client interface {
	InspectIP(ctx context.Context, containerName string) (string, error)
	InspectPaused(ctx context.Context, containerName string) (bool, error)
	Pause(ctx context.Context, containerName string) error
	Unpause(ctx context.Context, containerName string) error
}

type CLIClient struct{}

func NewCLIClient() *CLIClient {
	return &CLIClient{}
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
