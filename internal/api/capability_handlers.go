package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"clawsynapse/internal/adapter"
	"clawsynapse/pkg/types"
)

// capabilityQueryTimeout bounds the NATS round-trip for a capability query /
// set. On timeout the handler degrades (available:false / ok:false) but keeps
// HTTP 200 so the TrustMesh frontend can degrade gracefully.
const capabilityQueryTimeout = 5 * time.Second

// setCapabilityBody mirrors docs/capability-contract.md §3.5.
type setCapabilityBody struct {
	Target   string         `json:"target"` // skill | model | cron
	Action   string         `json:"action"`
	Skill    string         `json:"skill,omitempty"`
	FileIDs  []string       `json:"fileIds,omitempty"`
	Model    string         `json:"model,omitempty"`
	Provider map[string]any `json:"provider,omitempty"`
	Job      map[string]any `json:"job,omitempty"`
	JobID    string         `json:"jobId,omitempty"`
}

// toAnyMap converts a struct into map[string]any via JSON round-trip, used to
// embed typed capability payloads into types.APIResult.Data.
func toAnyMap(v any) map[string]any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// GET /v1/peers/{nodeId}/cron/executions?jobId=&limit= — query a peer node's
// scheduled-job execution history (state + result previews). Always HTTP 200;
// errors degrade to an empty list with an error field.
func (s *Server) handlePeerCronExecutions(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeId")
	if nodeID == "" {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:      false,
			Code:    "capability.invalid",
			Message: "nodeId is required",
			TS:      time.Now().UnixMilli(),
		})
		return
	}
	jobID := r.URL.Query().Get("jobId")
	limit := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}

	if s.capability == nil {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:   true,
			Code: "ok",
			Data: map[string]any{
				"executions": []any{},
				"error":      "capability.unavailable: capability service disabled",
			},
			TS: time.Now().UnixMilli(),
		})
		return
	}

	ctx, cancel := contextWithTimeout(r.Context(), capabilityQueryTimeout)
	defer cancel()

	resp, err := s.capability.ListExecutions(ctx, nodeID, jobID, limit)
	if err != nil {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:   true,
			Code: "ok",
			Data: map[string]any{
				"executions": []any{},
				"error":      "capability.timeout: " + err.Error(),
			},
			TS: time.Now().UnixMilli(),
		})
		return
	}

	respondJSON(w, http.StatusOK, types.APIResult{
		OK:   true,
		Code: "ok",
		Data: toAnyMap(resp),
		TS:   time.Now().UnixMilli(),
	})
}

// GET /v1/peers/{nodeId}/capabilities — query a peer node's capabilities over
// the NATS grid. Always HTTP 200; the body carries available/reason.
func (s *Server) handlePeerCapabilities(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeId")
	if nodeID == "" {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:      false,
			Code:    "capability.invalid",
			Message: "nodeId is required",
			TS:      time.Now().UnixMilli(),
		})
		return
	}

	if s.capability == nil {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:   true,
			Code: "ok",
			Data: map[string]any{
				"product":   "unknown",
				"available": false,
				"reason":    "capability.unavailable: capability service disabled",
			},
			TS: time.Now().UnixMilli(),
		})
		return
	}

	ctx, cancel := contextWithTimeout(r.Context(), capabilityQueryTimeout)
	defer cancel()

	resp, err := s.capability.Query(ctx, nodeID)
	if err != nil {
		// Timeout / peer offline / publish failure → degrade, HTTP still 200.
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:   true,
			Code: "ok",
			Data: map[string]any{
				"product":   "unknown",
				"available": false,
				"reason":    "capability.timeout: " + err.Error(),
			},
			TS: time.Now().UnixMilli(),
		})
		return
	}

	respondJSON(w, http.StatusOK, types.APIResult{
		OK:   true,
		Code: "ok",
		Data: toAnyMap(resp),
		TS:   time.Now().UnixMilli(),
	})
}

// POST /v1/peers/{nodeId}/capabilities — write back skill/model/cron to a
// peer node. Always HTTP 200; the body carries ok/error/restartStatus.
func (s *Server) handlePeerCapabilitySet(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("nodeId")
	if nodeID == "" {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:      false,
			Code:    "capability.invalid",
			Message: "nodeId is required",
			TS:      time.Now().UnixMilli(),
		})
		return
	}

	var body setCapabilityBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:      false,
			Code:    "capability.invalid",
			Message: "invalid json payload",
			TS:      time.Now().UnixMilli(),
		})
		return
	}

	if s.capability == nil {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:   true,
			Code: "ok",
			Data: map[string]any{
				"ok":            false,
				"target":        body.Target,
				"action":        body.Action,
				"restartStatus": "none",
				"error":         "capability.unavailable: capability service disabled",
			},
			TS: time.Now().UnixMilli(),
		})
		return
	}

	ctx, cancel := contextWithTimeout(r.Context(), capabilityQueryTimeout)
	defer cancel()

	resp, err := s.capability.Set(ctx, nodeID, &adapter.CapabilitySetRequest{
		Target:   body.Target,
		Action:   body.Action,
		Skill:    body.Skill,
		FileIDs:  body.FileIDs,
		Model:    body.Model,
		Provider: body.Provider,
		Job:      body.Job,
		JobID:    body.JobID,
	})
	if err != nil {
		respondJSON(w, http.StatusOK, types.APIResult{
			OK:   true,
			Code: "ok",
			Data: map[string]any{
				"ok":            false,
				"target":        body.Target,
				"action":        body.Action,
				"restartStatus": "none",
				"error":         "capability.timeout: " + err.Error(),
			},
			TS: time.Now().UnixMilli(),
		})
		return
	}

	respondJSON(w, http.StatusOK, types.APIResult{
		OK:   true,
		Code: "ok",
		Data: toAnyMap(resp),
		TS:   time.Now().UnixMilli(),
	})
}
