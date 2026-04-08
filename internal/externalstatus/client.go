package externalstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Reporter interface {
	Update(ctx context.Context, dockerContainerID string, status string) error
}

type Client struct {
	endpoint   string
	apiKey     string
	httpClient *http.Client
}

func NewClient(endpoint, apiKey string) *Client {
	return &Client{
		endpoint: strings.TrimSpace(endpoint),
		apiKey:   strings.TrimSpace(apiKey),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (c *Client) Update(ctx context.Context, dockerContainerID string, status string) error {
	if c == nil {
		return nil
	}
	if c.apiKey == "" || c.endpoint == "" {
		return nil
	}
	if dockerContainerID == "" {
		return errors.New("docker container id is required")
	}
	if status == "" {
		return errors.New("status is required")
	}
	payload := map[string]string{
		"dockerContainerId": dockerContainerID,
		"status":            status,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send status update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("status update failed: http %d", resp.StatusCode)
	}
	return nil
}
