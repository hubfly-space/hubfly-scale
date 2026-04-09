package externalresources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Reporter interface {
	Update(ctx context.Context, dockerContainerID string, cpu *float64, ramMB *int64) error
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

func (c *Client) Update(ctx context.Context, dockerContainerID string, cpu *float64, ramMB *int64) error {
	if c == nil {
		return nil
	}
	if c.apiKey == "" || c.endpoint == "" {
		return errors.New("external resource reporter not configured")
	}
	if dockerContainerID == "" {
		return errors.New("docker container id is required")
	}
	if cpu == nil && ramMB == nil {
		return errors.New("cpu or ram is required")
	}
	payload := map[string]any{
		"dockerContainerId": dockerContainerID,
	}
	if cpu != nil {
		payload["cpu"] = *cpu
	}
	if ramMB != nil {
		payload["ram"] = *ramMB
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
		return fmt.Errorf("send resource update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("resource update failed: http %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if err := checkSuccess(body); err != nil {
		return err
	}
	return nil
}

type apiResponse struct {
	Success *bool  `json:"success"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

func checkSuccess(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.Success != nil && !*resp.Success {
		msg := strings.TrimSpace(resp.Message)
		if msg == "" {
			msg = strings.TrimSpace(resp.Error)
		}
		if msg == "" {
			msg = "success=false"
		}
		return fmt.Errorf("resource update failed: %s", msg)
	}
	return nil
}
