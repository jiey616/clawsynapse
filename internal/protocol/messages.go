package protocol

type DiscoveryAnnounce struct {
	MessageID    string         `json:"messageId"`
	MessageType  string         `json:"messageType"`
	NodeID       string         `json:"nodeId"`
	DID          string         `json:"did,omitempty"`
	Version      string         `json:"version"`
	AgentProduct string         `json:"agentProduct"`
	Capabilities []string       `json:"capabilities"`
	Inbox        string         `json:"inbox"`
	PublicKey    string         `json:"publicKey"`
	Ts           int64          `json:"ts"`
	TTLms        int64          `json:"ttlMs"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	Signature    string         `json:"signature,omitempty"`
}

type DiscoveryDepart struct {
	MessageID   string `json:"messageId"`
	MessageType string `json:"messageType"`
	NodeID      string `json:"nodeId"`
	Reason      string `json:"reason,omitempty"`
	Ts          int64  `json:"ts"`
	Signature   string `json:"signature,omitempty"`
}

type AuthChallengeRequest struct {
	MessageID   string `json:"messageId"`
	MessageType string `json:"messageType"`
	From        string `json:"from"`
	To          string `json:"to"`
	PublicKey   string `json:"publicKey"`
	Nonce       string `json:"nonce"`
	Ts          int64  `json:"ts"`
	Alg         string `json:"alg"`
	Signature   string `json:"signature,omitempty"`
}

type AuthChallengeResponse struct {
	MessageID    string `json:"messageId"`
	MessageType  string `json:"messageType"`
	From         string `json:"from"`
	To           string `json:"to"`
	PublicKey    string `json:"publicKey"`
	Nonce        string `json:"nonce"`
	ChallengeRef string `json:"challengeRef"`
	Proof        string `json:"proof"`
	Ts           int64  `json:"ts"`
}

type AuthChallengeAck struct {
	MessageID    string `json:"messageId"`
	MessageType  string `json:"messageType"`
	From         string `json:"from"`
	To           string `json:"to"`
	ChallengeRef string `json:"challengeRef"`
	Proof        string `json:"proof"`
	Ts           int64  `json:"ts"`
}

type TrustRequest struct {
	MessageID    string   `json:"messageId"`
	MessageType  string   `json:"messageType"`
	From         string   `json:"from"`
	To           string   `json:"to"`
	RequestID    string   `json:"requestId"`
	Reason       string   `json:"reason,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Ts           int64    `json:"ts"`
	Signature    string   `json:"signature,omitempty"`
}

type TrustResponse struct {
	MessageID   string `json:"messageId"`
	MessageType string `json:"messageType"`
	From        string `json:"from"`
	To          string `json:"to"`
	RequestID   string `json:"requestId"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason,omitempty"`
	Ts          int64  `json:"ts"`
	Signature   string `json:"signature,omitempty"`
}

type TrustRevoke struct {
	MessageID   string `json:"messageId"`
	MessageType string `json:"messageType"`
	From        string `json:"from"`
	To          string `json:"to"`
	Reason      string `json:"reason,omitempty"`
	Ts          int64  `json:"ts"`
	Signature   string `json:"signature,omitempty"`
}

type TransferAvailable struct {
	MessageID   string `json:"messageId"`
	MessageType string `json:"messageType"`
	TransferID  string `json:"transferId"`
	From        string `json:"from"`
	To          string `json:"to"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	MimeType    string `json:"mimeType,omitempty"`
	Bucket      string `json:"bucket"`
	Ts          int64  `json:"ts"`
	Signature   string `json:"signature,omitempty"`
}

type MessageEnvelope struct {
	ID              string         `json:"id"`
	Type            string         `json:"type"`
	AgentID         string         `json:"agentId,omitempty"`
	From            string         `json:"from"`
	To              string         `json:"to,omitempty"`
	Content         string         `json:"content,omitempty"`
	SessionKey      string         `json:"sessionKey,omitempty"`
	Ts              int64          `json:"ts"`
	Sig             string         `json:"sig,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	ProtocolVersion string         `json:"protocolVersion,omitempty"`
}

// ── Capability (node capability query & write-back) ────────────────
// See docs/capability-contract.md. Subject:
//   clawsynapse.capability.<targetNodeId>.(query|response|set|set_response)

// SkillInfo describes a Hermes skill exposed by a node.
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
}

// ModelInfo describes a configured model provider. api_key is never echoed.
type ModelInfo struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	IsDefault bool   `json:"isDefault"`
}

// ExecutionInfo describes a single scheduled-job execution (state + result
// summary). The full markdown result is fetched via the executions detail
// query (see CapabilityExecutionsQuery).
type ExecutionInfo struct {
	ExecutionID   string `json:"executionId"`
	JobID         string `json:"jobId"`
	Status        string `json:"status"` // running | completed | failed | unknown
	StartedAt     int64  `json:"startedAtMs"`
	FinishedAt    int64  `json:"finishedAtMs,omitempty"`
	DurationMs    int64  `json:"durationMs,omitempty"`
	Error         string `json:"error,omitempty"`
	OutputFile    string `json:"outputFile,omitempty"`
	OutputPreview string `json:"outputPreview,omitempty"`
	// Output 携带完整结果 markdown；仅执行历史详情（capability.executions 第 2 层）返回，
	// capabilities 读回（第 1 层）不填充以保持列表轻量。
	Output string `json:"output,omitempty"`
}

// CronJobInfo describes a gateway scheduled job.
type CronJobInfo struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Schedule   string          `json:"schedule"`
	Enabled    bool            `json:"enabled"`
	Prompt     string          `json:"prompt"`
	Skills     []string        `json:"skills,omitempty"`
	NextRun    string          `json:"nextRun,omitempty"`
	Executions []ExecutionInfo `json:"executions,omitempty"`
}

// CapabilityQuery requests the capability list of the target node.
type CapabilityQuery struct {
	MessageID   string `json:"messageId"`
	MessageType string `json:"messageType"`
	From        string `json:"from"`
	To          string `json:"to"`
	RequestID   string `json:"requestId"`
	Ts          int64  `json:"ts"`
	Signature   string `json:"signature,omitempty"`
}

// CapabilityResponse carries the capability list back to the requester.
type CapabilityResponse struct {
	MessageID   string        `json:"messageId"`
	MessageType string        `json:"messageType"`
	From        string        `json:"from"`
	To          string        `json:"to"`
	RequestID   string        `json:"requestId"`
	Product     string        `json:"product"`
	Available   bool          `json:"available"`
	Skills      []SkillInfo   `json:"skills"`
	Models      []ModelInfo   `json:"models"`
	Jobs        []CronJobInfo `json:"jobs"`
	Reason      string        `json:"reason,omitempty"`
	Ts          int64         `json:"ts"`
	Signature   string        `json:"signature,omitempty"`
}

// CapabilitySet requests a write-back operation (skill/model/cron) on the
// target node.
type CapabilitySet struct {
	MessageID   string         `json:"messageId"`
	MessageType string         `json:"messageType"`
	From        string         `json:"from"`
	To          string         `json:"to"`
	RequestID   string         `json:"requestId"`
	Target      string         `json:"target"` // skill | model | cron
	Action      string         `json:"action"` // add|update|enable|disable|switch|delete|create|pause|resume|run
	Skill       string         `json:"skill,omitempty"`
	FileIDs     []string       `json:"fileIds,omitempty"`
	Model       string         `json:"model,omitempty"`
	Provider    map[string]any `json:"provider,omitempty"`
	Job         map[string]any `json:"job,omitempty"`
	JobID       string         `json:"jobId,omitempty"`
	Ts          int64          `json:"ts"`
	Signature   string         `json:"signature,omitempty"`
}

// CapabilitySetResponse carries the write-back result back to the requester.
type CapabilitySetResponse struct {
	MessageID     string `json:"messageId"`
	MessageType   string `json:"messageType"`
	From          string `json:"from"`
	To            string `json:"to"`
	RequestID     string `json:"requestId"`
	OK            bool   `json:"ok"`
	Target        string `json:"target"`
	Action        string `json:"action"`
	Skill         string `json:"skill,omitempty"`
	Model         string `json:"model,omitempty"`
	JobID         string `json:"jobId,omitempty"`
	RestartStatus string `json:"restartStatus"` // none | restarted | restart_failed
	Error         string `json:"error,omitempty"`
	Ts            int64  `json:"ts"`
	Signature     string `json:"signature,omitempty"`
}

// CapabilityExecutionsQuery requests scheduled-job execution history (and the
// full markdown result of each execution) for a job on the target node.
type CapabilityExecutionsQuery struct {
	MessageID   string `json:"messageId"`
	MessageType string `json:"messageType"`
	From        string `json:"from"`
	To          string `json:"to"`
	RequestID   string `json:"requestId"`
	JobID       string `json:"jobId,omitempty"` // empty = all jobs
	Limit       int    `json:"limit,omitempty"` // 0 = default (20)
	Ts          int64  `json:"ts"`
	Signature   string `json:"signature,omitempty"`
}

// CapabilityExecutionsResponse carries the execution history back.
type CapabilityExecutionsResponse struct {
	MessageID   string          `json:"messageId"`
	MessageType string          `json:"messageType"`
	From        string          `json:"from"`
	To          string          `json:"to"`
	RequestID   string          `json:"requestId"`
	Executions  []ExecutionInfo `json:"executions"`
	Error       string          `json:"error,omitempty"`
	Ts          int64           `json:"ts"`
	Signature   string          `json:"signature,omitempty"`
}
