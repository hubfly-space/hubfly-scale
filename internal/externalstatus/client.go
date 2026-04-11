package externalstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Reporter interface {
	Update(ctx context.Context, dockerContainerID string, status string) error
}

var ErrNotConfigured = errors.New("external status reporter not configured")

const maxLoggedBodyBytes = 2048

type Client struct {
	endpoint   string
	apiKey     string
	httpClient *http.Client
	logger     *log.Logger
}

func NewClient(endpoint, apiKey string, logger *log.Logger) *Client {
	return &Client{
		endpoint: strings.TrimSpace(endpoint),
		apiKey:   strings.TrimSpace(apiKey),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		logger: logger,
	}
}

func (c *Client) Update(ctx context.Context, dockerContainerID string, status string) error {
	if c == nil {
		return ErrNotConfigured
	}
	if c.apiKey == "" || c.endpoint == "" {
		return ErrNotConfigured
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

	if c.logger != nil {
		c.logger.Printf("external status request endpoint=%s container=%s status=%s", c.endpoint, dockerContainerID, status)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send status update: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if c.logger != nil {
		c.logger.Printf("external status response status=%d body=%q", resp.StatusCode, trimBody(respBody))
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("status update failed: http %d body=%s", resp.StatusCode, trimBody(respBody))
	}
	if err := checkSuccess(respBody); err != nil {
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
		return fmt.Errorf("status update failed: %s", msg)
	}
	return nil
}

func trimBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if len(body) <= maxLoggedBodyBytes {
		return string(body)
	}
	return string(body[:maxLoggedBodyBytes]) + "...(truncated)"
}
