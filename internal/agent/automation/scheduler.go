package automation

import (
	"context"
	"time"
)

// Scheduler 周期性调 Manager.Tick + Manager.Reconcile。
// 生命周期：Start 阻塞直到 ctx 取消；ctx 取消后 Start 返回，done channel 关闭。
// 调用方（bootstrap.App）在 Shutdown 时 cancel ctx，然后等 Wait 返回再 Close Store。
type Scheduler struct {
	manager  *Manager
	interval time.Duration
	done     chan struct{}
}

// NewScheduler 构造 Scheduler。interval <= 0 时使用 1 分钟默认。
func NewScheduler(manager *Manager, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Scheduler{
		manager:  manager,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start 阻塞运行直到 ctx 取消。Start 返回后 done channel 关闭。
// 同一个 Scheduler 只能 Start 一次；重复 Start 会 panic（close closed channel）。
func (s *Scheduler) Start(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// Tick/Reconcile 错误不 panic；下次 tick 继续。
			_ = s.manager.Tick(ctx, now)
			_ = s.manager.Reconcile(ctx)
		}
	}
}

// Wait 阻塞直到 Start 返回（ctx 取消后）。幂等：多次 Wait 都安全。
func (s *Scheduler) Wait() {
	<-s.done
}

// Stop 是 cancel + Wait 的便捷封装。cancel 由调用方持有；Stop 接收一个 ctx 仅
// 用于在 Wait 上叠加一个外部 deadline（若 ctx 在 Wait 返回前到期，返回 ctx.Err()）。
func (s *Scheduler) Stop(ctx context.Context) error {
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
