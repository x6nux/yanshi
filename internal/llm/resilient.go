package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// ResilientConfig tunes retry and failover for a provider chain.
type ResilientConfig struct {
	MaxRetries int           // retries per provider before failover (default 3)
	BaseDelay  time.Duration // backoff base (default 200ms)
	MaxDelay   time.Duration // backoff cap (default 5s)
}

// ResilientClient wraps an ordered chain of providers. Within a provider it
// retries on RetryableError with exponential backoff, then fails over to the
// next provider when retries are exhausted.
type ResilientClient struct {
	chain []Client
	cfg   ResilientConfig
}

// NewResilient builds a ResilientClient over an ordered provider chain.
func NewResilient(chain []Client, cfg ResilientConfig) (*ResilientClient, error) {
	if len(chain) == 0 {
		return nil, fmt.Errorf("llm: empty provider chain")
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 200 * time.Millisecond
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 5 * time.Second
	}
	return &ResilientClient{chain: chain, cfg: cfg}, nil
}

// Chat tries each provider in order, retrying within a provider on transient
// errors and failing over when a provider is exhausted.
func (c *ResilientClient) Chat(ctx context.Context, messages []Message) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	var lastErr error
	for _, p := range c.chain {
		resp, err := c.chatWithRetry(ctx, p, messages)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("llm: all providers failed")
	}
	return Response{}, fmt.Errorf("llm: chain exhausted: %w", lastErr)
}

func (c *ResilientClient) chatWithRetry(ctx context.Context, p Client, messages []Message) (Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Response{}, ctx.Err()
			case <-time.After(c.backoff(attempt)):
			}
		}
		resp, err := p.Chat(ctx, messages)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return Response{}, err // non-retryable: stop retrying this provider
		}
	}
	return Response{}, lastErr
}

func (c *ResilientClient) backoff(attempt int) time.Duration {
	d := time.Duration(float64(c.cfg.BaseDelay) * math.Pow(2, float64(attempt-1)))
	if d < 0 || d > c.cfg.MaxDelay {
		return c.cfg.MaxDelay
	}
	return d
}

func isRetryable(err error) bool {
	var re *RetryableError
	return errors.As(err, &re)
}
