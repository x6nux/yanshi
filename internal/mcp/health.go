package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// HealthConfig controls Manager background health checking and reconnection.
// Zero values fall back to DefaultHealthConfig() in SetHealthConfig.
type HealthConfig struct {
	// Enabled turns on the background health ticker. If false, StartHealthLoop
	// returns a no-op cancel func and steady-state reconnection is disabled.
	Enabled bool
	// Interval between health checks. Default 60s.
	Interval time.Duration
	// StartupTimeout caps the total time spent in startOne (Initialize +
	// tools/list). Default 30s. A server that does not become Ready within
	// this window is marked Failed with the underlying error.
	StartupTimeout time.Duration
	// ReconnectMaxAttempts caps reconnection attempts per failure. 0 = unlimited.
	ReconnectMaxAttempts int
	// ReconnectInitialBackoff is the first backoff delay between attempts.
	// Default 1s.
	ReconnectInitialBackoff time.Duration
	// ReconnectMaxBackoff caps the exponential backoff. Default 60s.
	ReconnectMaxBackoff time.Duration
}

// DefaultHealthConfig returns production-safe defaults.
func DefaultHealthConfig() HealthConfig {
	return HealthConfig{
		Enabled:                 true,
		Interval:                60 * time.Second,
		StartupTimeout:          30 * time.Second,
		ReconnectInitialBackoff: 1 * time.Second,
		ReconnectMaxBackoff:     60 * time.Second,
	}
}

// SetHealthConfig replaces the manager's health configuration. Apply before
// StartHealthLoop. Safe to call concurrently with other Manager methods.
func (m *Manager) SetHealthConfig(cfg HealthConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.Interval == 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = 30 * time.Second
	}
	if cfg.ReconnectInitialBackoff == 0 {
		cfg.ReconnectInitialBackoff = 1 * time.Second
	}
	if cfg.ReconnectMaxBackoff == 0 {
		cfg.ReconnectMaxBackoff = 60 * time.Second
	}
	m.health = cfg
}

// StartHealthLoop spawns a goroutine that pings each Ready server at
// m.health.Interval. A failed ping marks the server Failed and schedules a
// background reconnect with exponential backoff up to ReconnectMaxBackoff.
// The returned cancel func stops the loop but does not shut down clients
// (call Shutdown for that).
func (m *Manager) StartHealthLoop(ctx context.Context) context.CancelFunc {
	m.mu.Lock()
	cfg := m.health
	m.mu.Unlock()
	if !cfg.Enabled {
		return func() {}
	}
	loopCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				m.healthOnce(loopCtx)
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func (m *Manager) healthOnce(ctx context.Context) {
	m.mu.Lock()
	names := make([]string, 0, len(m.clients))
	for n := range m.clients {
		names = append(names, n)
	}
	m.mu.Unlock()
	for _, name := range names {
		m.mu.Lock()
		cli := m.clients[name]
		cfg := m.servers[name]
		m.mu.Unlock()
		if cli == nil || cfg == nil {
			continue
		}
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := cli.Ping(pingCtx)
		cancel()
		if err == nil {
			continue
		}
		m.markFailed(name, "ping: "+err.Error())
		if cfg.Reconnect {
			go func() { _ = m.reconnect(context.Background(), name) }()
		}
	}
}

// markFailed records the failure reason and status under the manager mutex.
// Safe to call concurrently with all Manager methods.
func (m *Manager) markFailed(name, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[name] = StatusFailed
	m.errs[name] = reason
}

// reconnect attempts to ring a failed/disconnected server back to Ready
// using exponential backoff. Single-flight per server: concurrent calls for
// the same name coalesce — only the first triggers startOne, others return a
// sentinel error. Returns nil on successful reconnection.
func (m *Manager) reconnect(ctx context.Context, name string) error {
	m.mu.Lock()
	if _, running := m.reconnectInflight[name]; running {
		m.mu.Unlock()
		return fmt.Errorf("mcp: reconnect already in flight for %q", name)
	}
	m.reconnectInflight[name] = struct{}{}
	cfg := m.servers[name]
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.reconnectInflight, name)
		m.mu.Unlock()
	}()
	if cfg == nil {
		return fmt.Errorf("mcp: unknown server %q", name)
	}
	if !cfg.Reconnect {
		return fmt.Errorf("mcp: server %q has reconnect disabled", name)
	}
	// Tear down any lingering client before retrying.
	_ = m.Disable(ctx, name)
	m.mu.Lock()
	cfgCopy := *cfg
	health := m.health
	m.mu.Unlock()
	backoff := health.ReconnectInitialBackoff
	attempt := 0
	for {
		attempt++
		if health.ReconnectMaxAttempts > 0 && attempt > health.ReconnectMaxAttempts {
			return fmt.Errorf("mcp: reconnect exhausted for %q after %d attempts", name, attempt-1)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		cli, err := m.startOne(ctx, &cfgCopy)
		if err == nil {
			m.installReadyClient(name, cli)
			return nil
		}
		m.markFailed(name, fmt.Sprintf("reconnect attempt %d: %v", attempt, err))
		backoff *= 2
		if backoff > health.ReconnectMaxBackoff {
			backoff = health.ReconnectMaxBackoff
		}
	}
}

// CallToolRetry is Manager.CallTool with a one-shot reconnect on failure.
// Use for ad-hoc external callers; steady-state coverage is the health loop.
func (m *Manager) CallToolRetry(ctx context.Context, qualified string, args []byte) (json.RawMessage, error) {
	res, err := m.CallTool(ctx, qualified, args)
	if err == nil {
		return res, nil
	}
	td, ok := m.LookupTool(ctx, qualified)
	if !ok {
		return nil, fmt.Errorf("mcp: tool %q not found: %w", qualified, err)
	}
	if reconnectErr := m.reconnect(ctx, td.ServerName); reconnectErr != nil {
		return nil, fmt.Errorf("mcp: tool %q call failed (%v); reconnect failed: %w", qualified, err, reconnectErr)
	}
	return m.CallTool(ctx, qualified, args)
}
