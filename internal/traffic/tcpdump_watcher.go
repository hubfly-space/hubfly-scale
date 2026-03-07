package traffic

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
)

type Watcher struct {
	logger *log.Logger
}

func NewWatcher(logger *log.Logger) *Watcher {
	return &Watcher{logger: logger}
}

// Start returns two channels:
// - packet event timestamps whenever tcpdump sees matching traffic
// - async errors from watcher process
func (w *Watcher) Start(ctx context.Context, ip string) (<-chan struct{}, <-chan error, error) {
	packetCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	args := []string{"-l", "-n", "-q", "-i", "any", "host", ip}
	cmd := exec.CommandContext(ctx, "tcpdump", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("tcpdump stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("tcpdump stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start tcpdump: %w", err)
	}

	go func() {
		defer close(packetCh)
		defer close(errCh)

		stderrScanner := bufio.NewScanner(stderr)
		go func() {
			for stderrScanner.Scan() {
				w.logger.Printf("tcpdump[%s] %s", ip, stderrScanner.Text())
			}
		}()

		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			select {
			case packetCh <- struct{}{}:
			default:
				// We only need edge notification; keep CPU and channel pressure low.
			}
		}

		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("scan tcpdump output: %w", err)
			return
		}
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("tcpdump exited: %w", err)
		}
	}()

	return packetCh, errCh, nil
}
