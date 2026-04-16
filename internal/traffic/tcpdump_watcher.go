package traffic

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type Watcher struct {
	logger *log.Logger

	mu            sync.RWMutex
	sessions      map[string]map[*watchSession]struct{}
	desiredFilter string
	filterCh      chan string
	ipRegex       *regexp.Regexp
}

type watchSession struct {
	ip       string
	packetCh chan struct{}
	errCh    chan error
}

func NewWatcher(logger *log.Logger) *Watcher {
	w := &Watcher{
		logger:   logger,
		sessions: make(map[string]map[*watchSession]struct{}),
		filterCh: make(chan string, 1),
		ipRegex:  regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`),
	}
	go w.captureLoop()
	return w
}

// Start returns two channels:
// - packet event timestamps whenever tcpdump sees matching traffic
// - async errors from watcher process
func (w *Watcher) Start(ctx context.Context, ip string) (<-chan struct{}, <-chan error, error) {
	if ip == "" {
		return nil, nil, fmt.Errorf("ip is required")
	}
	packetCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	session := &watchSession{
		ip:       ip,
		packetCh: packetCh,
		errCh:    errCh,
	}

	w.mu.Lock()
	set := w.sessions[ip]
	if set == nil {
		set = make(map[*watchSession]struct{})
		w.sessions[ip] = set
	}
	set[session] = struct{}{}
	w.updateFilterLocked()
	w.mu.Unlock()
	w.logger.Printf("watcher session added ip=%s", ip)

	go func() {
		<-ctx.Done()
		w.unregister(session)
	}()

	return packetCh, errCh, nil
}

func (w *Watcher) unregister(session *watchSession) {
	w.mu.Lock()
	set := w.sessions[session.ip]
	if set != nil {
		delete(set, session)
		if len(set) == 0 {
			delete(w.sessions, session.ip)
		}
	}
	w.updateFilterLocked()
	w.mu.Unlock()
	w.logger.Printf("watcher session removed ip=%s", session.ip)

	close(session.packetCh)
	close(session.errCh)
}

func (w *Watcher) updateFilterLocked() {
	filter := w.buildFilterLocked()
	if filter == w.desiredFilter {
		return
	}
	w.desiredFilter = filter
	select {
	case w.filterCh <- filter:
	default:
		<-w.filterCh
		w.filterCh <- filter
	}
}

func (w *Watcher) buildFilterLocked() string {
	if len(w.sessions) == 0 {
		return ""
	}
	ips := make([]string, 0, len(w.sessions))
	for ip := range w.sessions {
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	parts := make([]string, 0, len(ips)*2)
	for i, ip := range ips {
		if i > 0 {
			parts = append(parts, "or")
		}
		parts = append(parts, "host", ip)
	}
	return strings.Join(parts, " ")
}

func (w *Watcher) captureLoop() {
	var cancel context.CancelFunc
	for filter := range w.filterCh {
		if cancel != nil {
			cancel()
			cancel = nil
		}
		if filter == "" {
			continue
		}
		ctx, cancelFn := context.WithCancel(context.Background())
		cancel = cancelFn
		go w.runCapture(ctx, filter)
	}
}

func (w *Watcher) runCapture(ctx context.Context, filter string) {
	args := []string{"-l", "-n", "-q", "-i", "any"}
	args = append(args, strings.Fields(filter)...)
	w.logger.Printf("tcpdump starting filter=%q", filter)
	cmd := exec.CommandContext(ctx, "tcpdump", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		w.broadcastError(fmt.Errorf("tcpdump stdout pipe: %w", err))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		w.broadcastError(fmt.Errorf("tcpdump stderr pipe: %w", err))
		return
	}
	if err := cmd.Start(); err != nil {
		w.broadcastError(fmt.Errorf("start tcpdump: %w", err))
		return
	}

	stderrScanner := bufio.NewScanner(stderr)
	go func() {
		for stderrScanner.Scan() {
			w.logger.Printf("tcpdump %s", stderrScanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		for _, ip := range w.ipRegex.FindAllString(line, -1) {
			w.notifyIP(ip)
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		w.broadcastError(fmt.Errorf("scan tcpdump output: %w", err))
		return
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		w.broadcastError(fmt.Errorf("tcpdump exited: %w", err))
	}
}

func (w *Watcher) notifyIP(ip string) {
	w.mu.RLock()
	set := w.sessions[ip]
	if len(set) > 0 {
		w.logger.Printf("watcher packet matched ip=%s sessions=%d", ip, len(set))
	}
	for session := range set {
		select {
		case session.packetCh <- struct{}{}:
		default:
		}
	}
	w.mu.RUnlock()
}

func (w *Watcher) broadcastError(err error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, set := range w.sessions {
		for session := range set {
			select {
			case session.errCh <- err:
			default:
			}
		}
	}
}
