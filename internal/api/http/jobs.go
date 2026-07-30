package http

import (
	"time"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/shell"
)

// jobInfo converts a shell.Job to its proto representation. Time fields are
// passed through unixOrZero (Task 9); we DO NOT substitute time.Now() for
// missing start/end — stale jobs (Task 17 RestoreJobs) legitimately have
// zero times, and the UI renders "(no start time)" rather than fake a value.
//
// ExitCode is preserved verbatim (NOT omitempty on the wire — see
// proto.JobInfo.ExitCode): 0 is a meaningful clean-exit value that must
// survive the round trip.
func jobInfo(job shell.Job) proto.JobInfo {
	return proto.JobInfo{
		ID:        job.ID,
		SessionID: job.SessionID,
		Command:   job.Command,
		State:     string(job.State),
		Output:    job.Output,
		Error:     job.Error,
		ExitCode:  job.ExitCode,
		PID:       job.PID,
		StartedAt: proto.UnixOrZero(job.StartedAt),
		EndedAt:   unixOrZeroPtr(job.EndedAt),
	}
}

// unixOrZeroPtr handles the *time.Time shape shell.Job.EndedAt uses (Task 16
// picked the pointer form so an unset EndedAt serializes as absent via
// omitempty, not as the negative value time.Time{}.Unix() would produce on
// some platforms). Nil → 0; non-nil → proto.UnixOrZero(*t).
func unixOrZeroPtr(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return proto.UnixOrZero(*t)
}
