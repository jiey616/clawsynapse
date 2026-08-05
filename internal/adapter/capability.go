package adapter

import (
	"context"
	"strings"

	"clawsynapse/internal/protocol"
)

// CapabilityProvider is an optional capability implemented by adapters that
// can expose and/or mutate node capabilities (currently only Hermes).
//
// The capability service discovers it via a type assertion; adapters that do
// not implement it are reported as `available:false` / write-back rejected.
// Keeping this a separate interface avoids touching the six existing adapters.
type CapabilityProvider interface {
	// Capabilities returns the current node capability list
	// (skills / models / jobs).
	Capabilities(ctx context.Context) (*CapabilitiesResult, error)

	// ApplyCapabilitySet applies a write-back operation targeting
	// skill / model / cron.
	ApplyCapabilitySet(ctx context.Context, req *CapabilitySetRequest) (*CapabilitySetResult, error)

	// ListExecutions returns scheduled-job execution history (with result
	// previews) for jobID (empty = all jobs). Used to surface cron execution
	// records and results to the TrustMesh frontend.
	ListExecutions(ctx context.Context, jobID string, limit int) ([]protocol.ExecutionInfo, error)
}

// CapabilitiesResult aggregates skills / models / jobs for the capability
// response. Available=false (with Reason) means the read could not be served.
type CapabilitiesResult struct {
	Product   string
	Available bool
	Skills    []protocol.SkillInfo
	Models    []protocol.ModelInfo
	Jobs      []protocol.CronJobInfo
	Reason    string
}

// CapabilitySetRequest is the normalized write-back request handed to the
// adapter.
//
// For skill add/update, FileLocalPaths is resolved from fileIds by the
// capability service (via the transfer store) — the adapter must never accept
// arbitrary remote paths. Each entry is an absolute local path to a file that
// will be placed inside the managed skill directory.
type CapabilitySetRequest struct {
	Target         string
	Action         string
	Skill          string
	FileIDs        []string
	FileLocalPaths []string
	Model          string
	Provider       map[string]any
	Job            map[string]any
	JobID          string
}

// CapabilitySetResult reports the outcome of a write-back operation.
//
// RestartStatus is one of: "none" (cron, or rejected before restart),
// "restarted", "restart_failed".
type CapabilitySetResult struct {
	OK            bool
	Target        string
	Action        string
	Skill         string
	Model         string
	JobID         string
	RestartStatus string
	Error         string
}

// strAny extracts a trimmed string from an arbitrary map value (config.yaml
// fields, gateway JSON payloads). Returns "" for nil / non-string values.
func strAny(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// boolAny extracts a boolean from an arbitrary map value.
func boolAny(v any) bool {
	b, _ := v.(bool)
	return b
}
