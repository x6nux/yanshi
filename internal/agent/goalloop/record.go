package goalloop

// RunRecord is the durable summary of one goal-loop run, written to the store's
// kv table by cmd/yanshi after Run returns (G02: 持久化停止原因). It is
// pure data with JSON tags so it survives a kv round-trip without a schema
// migration — the kv table already holds arbitrary string values.
type RunRecord struct {
	Tier       string `json:"tier"`
	Complete   bool   `json:"complete"`
	StopReason string `json:"stop_reason"`
	Summary    string `json:"summary"`
	Iterations int    `json:"iterations"`
	Usage      Usage  `json:"usage"`
}

// NewRunRecord assembles a RunRecord from a finished run. tier is the resolved
// Tier the run was dispatched at; decision is the Loop's (or lightweight path's)
// terminal Decision; usage is the final accumulated spend; iterations is the
// number of plan-implement-evaluate-judge cycles executed (1 for the
// lightweight single-turn path).
func NewRunRecord(tier Tier, decision Decision, usage Usage, iterations int) RunRecord {
	return RunRecord{
		Tier:       tier.String(),
		Complete:   decision.Complete,
		StopReason: decision.StopReason,
		Summary:    decision.Summary,
		Iterations: iterations,
		Usage:      usage,
	}
}
