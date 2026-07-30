package automation

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// ParseSchedule 解析 kind/expression 为 Schedule。kind ∈ {"cron","interval"}.
//   - cron: 5-field standard parser（cron.ParseStandard），无秒字段。
//   - interval: 整数秒，>0；拒绝 0/负数/非数字。
//
// 解析成功的 Schedule 可以调用 Next 计算下一次运行时间。
func ParseSchedule(kind, expression string) (Schedule, error) {
	switch kind {
	case "cron":
		if expression == "" {
			return Schedule{}, errors.New("cron expression is empty")
		}
		if _, err := cron.ParseStandard(expression); err != nil {
			return Schedule{}, fmt.Errorf("invalid cron: %w", err)
		}
		return Schedule{Kind: "cron", Cron: expression}, nil
	case "interval":
		var seconds int64
		if _, err := fmt.Sscan(expression, &seconds); err != nil || seconds <= 0 {
			return Schedule{}, errors.New("interval must be a positive number of seconds")
		}
		return Schedule{Kind: "interval", IntervalSec: seconds}, nil
	default:
		return Schedule{}, fmt.Errorf("unsupported schedule kind %q", kind)
	}
}

// Next 返回严格晚于 after 的下一次运行时间。
func (s Schedule) Next(after time.Time) (time.Time, error) {
	switch s.Kind {
	case "cron":
		parsed, err := cron.ParseStandard(s.Cron)
		if err != nil {
			return time.Time{}, err
		}
		return parsed.Next(after), nil
	case "interval":
		if s.IntervalSec <= 0 {
			return time.Time{}, errors.New("interval must be positive")
		}
		return after.Add(time.Duration(s.IntervalSec) * time.Second), nil
	default:
		return time.Time{}, errors.New("unknown schedule kind")
	}
}
