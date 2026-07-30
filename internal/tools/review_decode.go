package tools

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type reviewFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Rule     string `json:"rule,omitempty"`
}

type reviewSubAgentOutput struct {
	Findings []reviewFinding `json:"findings"`
}

type reviewResult struct {
	Findings       []reviewFinding `json:"findings"`
	ChunksReviewed int             `json:"chunks_reviewed"`
	ArtifactRef    string          `json:"artifact_ref,omitempty"`
	Degraded       bool            `json:"degraded,omitempty"`
}

// decodeReviewSubAgentOutput parses the JSON a sub-agent returned. Sub-agents
// occasionally wrap output in prose or return a fence; extract the first
// balanced JSON object before unmarshalling.
func decodeReviewSubAgentOutput(raw string) (reviewSubAgentOutput, error) {
	var out reviewSubAgentOutput
	payload := extractJSONObject(raw)
	if payload == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return out, err
	}
	return out, nil
}

// extractJSONObject returns the substring of `s` from the first '{' to its
// matching '}'. Empty string if none found. Tolerates nested braces and
// strings containing braces.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' {
			escape = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// dedupeAndSortFindings removes duplicates (same File+Line+Severity+Message)
// and sorts by Severity (high > medium > low > info), then File, then Line.
func dedupeAndSortFindings(in []reviewFinding) []reviewFinding {
	seen := make(map[string]bool, len(in))
	out := make([]reviewFinding, 0, len(in))
	for _, f := range in {
		key := f.File + "|" + strconv.Itoa(f.Line) + "|" + f.Severity + "|" + f.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	sevRank := map[string]int{"high": 0, "medium": 1, "low": 2, "info": 3}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := sevRank[out[i].Severity], sevRank[out[j].Severity]
		if ri != rj {
			return ri < rj
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}
