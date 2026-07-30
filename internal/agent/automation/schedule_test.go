package automation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

func TestParseSchedule_CronValid(t *testing.T) {
	s, err := automation.ParseSchedule("cron", "*/5 * * * *")
	require.NoError(t, err)
	assert.Equal(t, "cron", s.Kind)
	assert.Equal(t, "*/5 * * * *", s.Cron)

	after := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	next, err := s.Next(after)
	require.NoError(t, err)
	assert.True(t, next.After(after), "next must be strictly after input")
}

func TestParseSchedule_CronInvalid(t *testing.T) {
	cases := []string{"", "not-a-cron", "99 99 99 99 99"}
	for _, in := range cases {
		_, err := automation.ParseSchedule("cron", in)
		require.Errorf(t, err, "cron %q must be rejected", in)
	}
}

func TestParseSchedule_IntervalPositive(t *testing.T) {
	s, err := automation.ParseSchedule("interval", "60")
	require.NoError(t, err)
	assert.Equal(t, int64(60), s.IntervalSec)

	after := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	next, err := s.Next(after)
	require.NoError(t, err)
	assert.Equal(t, after.Add(60*time.Second), next)
}

func TestParseSchedule_IntervalRejectsNonPositive(t *testing.T) {
	cases := []string{"0", "-1", "abc", ""}
	for _, in := range cases {
		_, err := automation.ParseSchedule("interval", in)
		require.Errorf(t, err, "interval %q must be rejected", in)
	}
}

func TestParseSchedule_UnknownKind(t *testing.T) {
	_, err := automation.ParseSchedule("hourly", "1")
	require.Error(t, err)
}
