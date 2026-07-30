package automation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

func TestMapTaskStatus_Table(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pending", automation.RunQueued},
		{"running", automation.RunRunning},
		{"completed", automation.RunCompleted},
		{"failed", automation.RunFailed},
		{"cancelled", automation.RunCanceled}, // A2 double-l → C1 single-l
		{"timeout", automation.RunFailed},     // broker-only，无 cancelled 等价
	}
	for _, c := range cases {
		got, ok := automation.MapTaskStatus[c.in]
		if !ok {
			t.Errorf("MapTaskStatus[%q] missing", c.in)
			continue
		}
		assert.Equal(t, c.want, got, "MapTaskStatus[%q]", c.in)
	}
}

func TestMapTaskStatus_DoesNotContainSingleLCancelled(t *testing.T) {
	// 防回归：表中不能误把 "canceled"（单-l）当作 key，那是 C1 自己的输出词汇。
	if _, ok := automation.MapTaskStatus["canceled"]; ok {
		t.Fatal(`MapTaskStatus must not contain single-l "canceled" as a key; that is C1's output vocabulary`)
	}
}

func TestMapTaskStatus_DoesNotContainDoubleLCancelledAsValue(t *testing.T) {
	for k, v := range automation.MapTaskStatus {
		if v == "cancelled" {
			t.Fatalf(`MapTaskStatus[%q] = "cancelled" (double-l); C1 outputs use single-l "canceled"`, k)
		}
	}
}
