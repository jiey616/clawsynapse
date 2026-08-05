// Package capability implements the capability.query / capability.set
// protocol module: querying and writing back a node's skills / models /
// scheduled jobs. See docs/capability-contract.md.
package capability

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"clawsynapse/internal/adapter"
	"clawsynapse/internal/discovery"
	"clawsynapse/internal/identity"
	"clawsynapse/internal/logging"
	"clawsynapse/internal/natsbus"
	"clawsynapse/internal/protocol"
	"clawsynapse/internal/transfer"
	"clawsynapse/pkg/types"
)

// opTimeout bounds the adapter work triggered by an inbound query/set
// (gateway reads, config edits, gateway restart).
const opTimeout = 30 * time.Second

// pendingRequest correlates an outbound capability query with its response.
// The API layer waits on the matching channel with a caller-supplied timeout.
type pendingRequest struct {
	resultCh chan *protocol.CapabilityResponse
	setCh    chan *protocol.CapabilitySetResponse
	execCh   chan *protocol.CapabilityExecutionsResponse
}

type Service struct {
	log      *slog.Logger
	bus      *natsbus.Client
	peers    *discovery.Registry
	transfer *transfer.Service
	adapter  adapter.AgentAdapter
	nodeID   string
	id       *identity.Identity

	mu      sync.Mutex
	pending map[string]*pendingRequest
}

func NewService(log *slog.Logger, bus *natsbus.Client, peers *discovery.Registry, transferSvc *transfer.Service, agentAdapter adapter.AgentAdapter, nodeID string, id *identity.Identity) *Service {
	return &Service{
		log:      log,
		bus:      bus,
		peers:    peers,
		transfer: transferSvc,
		adapter:  agentAdapter,
		nodeID:   nodeID,
		id:       id,
		pending:  map[string]*pendingRequest{},
	}
}

// Start registers the six capability subjects scoped to this node.
func (s *Service) Start() error {
	if _, err := s.bus.Subscribe("clawsynapse.capability."+s.nodeID+".query", s.handleCapabilityQuery); err != nil {
		return err
	}
	if _, err := s.bus.Subscribe("clawsynapse.capability."+s.nodeID+".set", s.handleCapabilitySet); err != nil {
		return err
	}
	if _, err := s.bus.Subscribe("clawsynapse.capability."+s.nodeID+".response", s.handleCapabilityResponse); err != nil {
		return err
	}
	if _, err := s.bus.Subscribe("clawsynapse.capability."+s.nodeID+".set_response", s.handleCapabilitySetResponse); err != nil {
		return err
	}
	if _, err := s.bus.Subscribe("clawsynapse.capability."+s.nodeID+".executions", s.handleCapabilityExecutions); err != nil {
		return err
	}
	if _, err := s.bus.Subscribe("clawsynapse.capability."+s.nodeID+".executions_response", s.handleCapabilityExecutionsResponse); err != nil {
		return err
	}
	return nil
}

// ── Outbound: query / set (used by the local HTTP API) ─────────────

// Query sends capability.query to targetNode and waits for capability.response
// (bounded by ctx; the API layer applies a 5s timeout). On timeout/offline the
// caller degrades to available:false.
func (s *Service) Query(ctx context.Context, targetNode string) (*protocol.CapabilityResponse, error) {
	targetNode = strings.TrimSpace(targetNode)
	if targetNode == "" {
		return nil, errors.New("targetNode is required")
	}

	reqID := randID()
	req := protocol.CapabilityQuery{
		MessageID:   randID(),
		MessageType: "capability.query",
		From:        s.nodeID,
		To:          targetNode,
		RequestID:   reqID,
		Ts:          time.Now().UnixMilli(),
	}
	req.Signature = s.signQuery(req)

	ch := make(chan *protocol.CapabilityResponse, 1)
	s.mu.Lock()
	s.pending[reqID] = &pendingRequest{resultCh: ch}
	s.mu.Unlock()
	defer s.clearPending(reqID)

	if err := s.bus.PublishJSON("clawsynapse.capability."+targetNode+".query", req); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		return resp, nil
	}
}

// Set sends capability.set to targetNode and waits for capability.set_response
// (bounded by ctx).
func (s *Service) Set(ctx context.Context, targetNode string, req *adapter.CapabilitySetRequest) (*protocol.CapabilitySetResponse, error) {
	targetNode = strings.TrimSpace(targetNode)
	if targetNode == "" {
		return nil, errors.New("targetNode is required")
	}

	msg := protocol.CapabilitySet{
		MessageID:   randID(),
		MessageType: "capability.set",
		From:        s.nodeID,
		To:          targetNode,
		RequestID:   randID(),
		Target:      req.Target,
		Action:      req.Action,
		Skill:       req.Skill,
		FileIDs:     req.FileIDs,
		Model:       req.Model,
		Provider:    req.Provider,
		Job:         req.Job,
		JobID:       req.JobID,
		Ts:          time.Now().UnixMilli(),
	}
	msg.Signature = s.signSet(msg)

	ch := make(chan *protocol.CapabilitySetResponse, 1)
	s.mu.Lock()
	s.pending[msg.RequestID] = &pendingRequest{setCh: ch}
	s.mu.Unlock()
	defer s.clearPending(msg.RequestID)

	if err := s.bus.PublishJSON("clawsynapse.capability."+targetNode+".set", msg); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		return resp, nil
	}
}

// ListExecutions sends capability.executions to targetNode and waits for the
// execution history (bounded by ctx).
func (s *Service) ListExecutions(ctx context.Context, targetNode, jobID string, limit int) (*protocol.CapabilityExecutionsResponse, error) {
	targetNode = strings.TrimSpace(targetNode)
	if targetNode == "" {
		return nil, errors.New("targetNode is required")
	}
	reqID := randID()
	msg := protocol.CapabilityExecutionsQuery{
		MessageID:   randID(),
		MessageType: "capability.executions",
		From:        s.nodeID,
		To:          targetNode,
		RequestID:   reqID,
		JobID:       jobID,
		Limit:       limit,
		Ts:          time.Now().UnixMilli(),
	}
	msg.Signature = s.signExecutionsQuery(msg)

	ch := make(chan *protocol.CapabilityExecutionsResponse, 1)
	s.mu.Lock()
	s.pending[reqID] = &pendingRequest{execCh: ch}
	s.mu.Unlock()
	defer s.clearPending(reqID)

	if err := s.bus.PublishJSON("clawsynapse.capability."+targetNode+".executions", msg); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		return resp, nil
	}
}

func (s *Service) clearPending(reqID string) {
	s.mu.Lock()
	delete(s.pending, reqID)
	s.mu.Unlock()
}

// ── Inbound: query / set handlers ──────────────────────────────────

func (s *Service) handleCapabilityQuery(subject string, data []byte) {
	var req protocol.CapabilityQuery
	if err := json.Unmarshal(data, &req); err != nil {
		s.log.Warn("decode capability.query failed", logging.Subject(subject), logging.Error(err))
		return
	}
	if req.To != s.nodeID {
		return
	}
	if err := protocol.ValidateMessage(subject, protocol.ControlMessage{MessageType: req.MessageType, To: req.To, Ts: req.Ts}, protocol.ValidateOptions{}); err != nil {
		s.log.Warn("invalid capability.query", logging.Error(err))
		return
	}
	if !s.authorizedPeer(req.From, s.querySignatureInput(req), req.Signature) {
		s.sendQueryDenied(req.From, req.RequestID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	provider, ok := s.adapter.(adapter.CapabilityProvider)
	if !ok {
		s.sendCapabilityResponse(req.From, req.RequestID, &protocol.CapabilityResponse{
			Product:   s.productName(),
			Available: false,
			Reason:    "capability.unavailable: adapter does not support capabilities",
		})
		return
	}

	res, err := provider.Capabilities(ctx)
	if err != nil {
		s.sendCapabilityResponse(req.From, req.RequestID, &protocol.CapabilityResponse{
			Product:   s.productName(),
			Available: false,
			Reason:    "capability.unavailable: " + err.Error(),
		})
		return
	}

	// Normalize nil slices to empty arrays so the JSON response is [] not null.
	skills := res.Skills
	models := res.Models
	jobs := res.Jobs
	if skills == nil {
		skills = []protocol.SkillInfo{}
	}
	if models == nil {
		models = []protocol.ModelInfo{}
	}
	if jobs == nil {
		jobs = []protocol.CronJobInfo{}
	}

	s.sendCapabilityResponse(req.From, req.RequestID, &protocol.CapabilityResponse{
		Product:   res.Product,
		Available: res.Available,
		Skills:    skills,
		Models:    models,
		Jobs:      jobs,
		Reason:    res.Reason,
	})
}

func (s *Service) handleCapabilitySet(subject string, data []byte) {
	var req protocol.CapabilitySet
	if err := json.Unmarshal(data, &req); err != nil {
		s.log.Warn("decode capability.set failed", logging.Subject(subject), logging.Error(err))
		return
	}
	if req.To != s.nodeID {
		return
	}
	if err := protocol.ValidateMessage(subject, protocol.ControlMessage{MessageType: req.MessageType, To: req.To, Ts: req.Ts}, protocol.ValidateOptions{}); err != nil {
		s.log.Warn("invalid capability.set", logging.Error(err))
		return
	}
	if !s.authorizedPeer(req.From, s.setSignatureInput(req), req.Signature) {
		s.sendSetDenied(req.From, req.RequestID, req.Target, req.Action)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	adapterReq := &adapter.CapabilitySetRequest{
		Target:   req.Target,
		Action:   req.Action,
		Skill:    req.Skill,
		FileIDs:  req.FileIDs,
		Model:    req.Model,
		Provider: req.Provider,
		Job:      req.Job,
		JobID:    req.JobID,
	}

	// Resolve fileIds → local paths for skill add/update via the transfer
	// store. The adapter only ever sees local absolute paths — it never
	// accepts arbitrary remote paths.
	if req.Target == "skill" && (req.Action == "add" || req.Action == "update") {
		for _, fid := range req.FileIDs {
			info, ok := s.transfer.GetTransfer(fid)
			if !ok || strings.TrimSpace(info.LocalPath) == "" {
				s.sendSetResponse(req.From, req.RequestID, &protocol.CapabilitySetResponse{
					OK: false, Target: req.Target, Action: req.Action,
					Skill: req.Skill, RestartStatus: "none",
					Error: "capability.invalid: fileId not found locally: " + fid,
				})
				return
			}
			adapterReq.FileLocalPaths = append(adapterReq.FileLocalPaths, info.LocalPath)
		}
	}

	s.logAudit(req)

	provider, ok := s.adapter.(adapter.CapabilityProvider)
	if !ok {
		s.sendSetResponse(req.From, req.RequestID, &protocol.CapabilitySetResponse{
			OK: false, Target: req.Target, Action: req.Action,
			Skill: req.Skill, Model: req.Model, JobID: req.JobID,
			RestartStatus: "none",
			Error:         "capability.unavailable: adapter does not support capabilities",
		})
		return
	}

	res, err := provider.ApplyCapabilitySet(ctx, adapterReq)
	if err != nil {
		s.sendSetResponse(req.From, req.RequestID, &protocol.CapabilitySetResponse{
			OK: false, Target: req.Target, Action: req.Action,
			Skill: req.Skill, Model: req.Model, JobID: req.JobID,
			RestartStatus: "none",
			Error:         "capability.unavailable: " + err.Error(),
		})
		return
	}

	s.sendSetResponse(req.From, req.RequestID, &protocol.CapabilitySetResponse{
		OK:            res.OK,
		Target:        res.Target,
		Action:        res.Action,
		Skill:         res.Skill,
		Model:         res.Model,
		JobID:         res.JobID,
		RestartStatus: res.RestartStatus,
		Error:         res.Error,
	})
}

// ── Inbound: response handlers (correlate by requestId) ────────────

func (s *Service) handleCapabilityResponse(subject string, data []byte) {
	var resp protocol.CapabilityResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		s.log.Warn("decode capability.response failed", logging.Subject(subject), logging.Error(err))
		return
	}
	if resp.To != s.nodeID {
		return
	}
	if err := protocol.ValidateMessage(subject, protocol.ControlMessage{MessageType: resp.MessageType, To: resp.To, Ts: resp.Ts}, protocol.ValidateOptions{}); err != nil {
		s.log.Warn("invalid capability.response", logging.Error(err))
		return
	}
	if !s.authorizedPeer(resp.From, s.responseSignatureInput(resp), resp.Signature) {
		return
	}

	s.mu.Lock()
	p, ok := s.pending[resp.RequestID]
	if ok && p.resultCh != nil {
		p.resultCh <- &resp
	}
	s.mu.Unlock()
}

func (s *Service) handleCapabilitySetResponse(subject string, data []byte) {
	var resp protocol.CapabilitySetResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		s.log.Warn("decode capability.set_response failed", logging.Subject(subject), logging.Error(err))
		return
	}
	if resp.To != s.nodeID {
		return
	}
	if err := protocol.ValidateMessage(subject, protocol.ControlMessage{MessageType: resp.MessageType, To: resp.To, Ts: resp.Ts}, protocol.ValidateOptions{}); err != nil {
		s.log.Warn("invalid capability.set_response", logging.Error(err))
		return
	}
	if !s.authorizedPeer(resp.From, s.setResponseSignatureInput(resp), resp.Signature) {
		return
	}

	s.mu.Lock()
	p, ok := s.pending[resp.RequestID]
	if ok && p.setCh != nil {
		p.setCh <- &resp
	}
	s.mu.Unlock()
}

// ── Inbound: executions query / response ───────────────────────────

func (s *Service) handleCapabilityExecutions(subject string, data []byte) {
	var req protocol.CapabilityExecutionsQuery
	if err := json.Unmarshal(data, &req); err != nil {
		s.log.Warn("decode capability.executions failed", logging.Subject(subject), logging.Error(err))
		return
	}
	if req.To != s.nodeID {
		return
	}
	if err := protocol.ValidateMessage(subject, protocol.ControlMessage{MessageType: req.MessageType, To: req.To, Ts: req.Ts}, protocol.ValidateOptions{}); err != nil {
		s.log.Warn("invalid capability.executions", logging.Error(err))
		return
	}
	if !s.authorizedPeer(req.From, s.executionsQuerySignatureInput(req), req.Signature) {
		s.sendExecutionsError(req.From, req.RequestID, "capability.denied")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	provider, ok := s.adapter.(adapter.CapabilityProvider)
	if !ok {
		s.sendExecutionsError(req.From, req.RequestID, "capability.unavailable: adapter does not support capabilities")
		return
	}

	execs, err := provider.ListExecutions(ctx, req.JobID, req.Limit)
	if err != nil {
		s.sendExecutionsError(req.From, req.RequestID, "capability.unavailable: "+err.Error())
		return
	}
	if execs == nil {
		execs = []protocol.ExecutionInfo{}
	}
	s.sendExecutionsResponse(req.From, req.RequestID, execs)
}

func (s *Service) handleCapabilityExecutionsResponse(subject string, data []byte) {
	var resp protocol.CapabilityExecutionsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		s.log.Warn("decode capability.executions_response failed", logging.Subject(subject), logging.Error(err))
		return
	}
	if resp.To != s.nodeID {
		return
	}
	if err := protocol.ValidateMessage(subject, protocol.ControlMessage{MessageType: resp.MessageType, To: resp.To, Ts: resp.Ts}, protocol.ValidateOptions{}); err != nil {
		s.log.Warn("invalid capability.executions_response", logging.Error(err))
		return
	}
	if !s.authorizedPeer(resp.From, s.executionsResponseSignatureInput(resp), resp.Signature) {
		return
	}

	s.mu.Lock()
	p, ok := s.pending[resp.RequestID]
	if ok && p.execCh != nil {
		p.execCh <- &resp
	}
	s.mu.Unlock()
}

func (s *Service) sendExecutionsResponse(to, requestID string, execs []protocol.ExecutionInfo) {
	resp := &protocol.CapabilityExecutionsResponse{
		MessageID:   randID(),
		MessageType: "capability.executions_response",
		From:        s.nodeID,
		To:          to,
		RequestID:   requestID,
		Executions:  execs,
		Ts:          time.Now().UnixMilli(),
	}
	resp.Signature = s.signExecutionsResponse(*resp)
	if err := s.bus.PublishJSON("clawsynapse.capability."+to+".executions_response", resp); err != nil {
		s.log.Warn("publish capability.executions_response failed", logging.Peer(to), logging.RequestID(requestID), logging.Error(err))
	}
}

func (s *Service) sendExecutionsError(to, requestID, msg string) {
	resp := &protocol.CapabilityExecutionsResponse{
		MessageID:   randID(),
		MessageType: "capability.executions_response",
		From:        s.nodeID,
		To:          to,
		RequestID:   requestID,
		Executions:  []protocol.ExecutionInfo{},
		Error:       msg,
		Ts:          time.Now().UnixMilli(),
	}
	resp.Signature = s.signExecutionsResponse(*resp)
	if err := s.bus.PublishJSON("clawsynapse.capability."+to+".executions_response", resp); err != nil {
		s.log.Warn("publish capability.executions_response failed", logging.Peer(to), logging.RequestID(requestID), logging.Error(err))
	}
}

// ── Response senders ───────────────────────────────────────────────

func (s *Service) sendCapabilityResponse(to, requestID string, resp *protocol.CapabilityResponse) {
	resp.MessageID = randID()
	resp.MessageType = "capability.response"
	resp.From = s.nodeID
	resp.To = to
	resp.RequestID = requestID
	resp.Ts = time.Now().UnixMilli()
	resp.Signature = s.signResponse(*resp)
	if err := s.bus.PublishJSON("clawsynapse.capability."+to+".response", resp); err != nil {
		s.log.Warn("publish capability.response failed", logging.Peer(to), logging.RequestID(requestID), logging.Error(err))
	}
}

func (s *Service) sendSetResponse(to, requestID string, resp *protocol.CapabilitySetResponse) {
	resp.MessageID = randID()
	resp.MessageType = "capability.set_response"
	resp.From = s.nodeID
	resp.To = to
	resp.RequestID = requestID
	resp.Ts = time.Now().UnixMilli()
	resp.Signature = s.signSetResponse(*resp)
	if err := s.bus.PublishJSON("clawsynapse.capability."+to+".set_response", resp); err != nil {
		s.log.Warn("publish capability.set_response failed", logging.Peer(to), logging.RequestID(requestID), logging.Error(err))
	}
}

func (s *Service) sendQueryDenied(to, requestID string) {
	s.sendCapabilityResponse(to, requestID, &protocol.CapabilityResponse{
		Available: false,
		Reason:    "capability.denied",
	})
}

func (s *Service) sendSetDenied(to, requestID, target, action string) {
	s.sendSetResponse(to, requestID, &protocol.CapabilitySetResponse{
		OK: false, Target: target, Action: action,
		RestartStatus: "none",
		Error:         "capability.denied",
	})
}

// ── Auth helpers ───────────────────────────────────────────────────

// authorizedPeer verifies the peer is known, trusted, and the message
// signature is valid against its public key.
func (s *Service) authorizedPeer(from, sigInput, signature string) bool {
	peer, ok := s.peers.Get(from)
	if !ok {
		s.log.Warn("capability from unknown peer", logging.Peer(from))
		return false
	}
	if peer.TrustStatus != types.TrustTrusted {
		s.log.Warn("capability from untrusted peer", logging.Peer(from), slog.String("trust", peer.TrustStatus))
		return false
	}
	pub, err := peerPublicKey(peer)
	if err != nil {
		s.log.Warn("capability peer public key unavailable", logging.Peer(from), logging.Error(err))
		return false
	}
	if !identity.Verify(pub, []byte(sigInput), signature) {
		s.log.Warn("capability invalid signature", logging.Peer(from))
		return false
	}
	return true
}

func peerPublicKey(peer types.Peer) ([]byte, error) {
	if peer.Metadata == nil {
		return nil, errors.New("peer metadata is empty")
	}
	v, ok := peer.Metadata["publicKey"].(string)
	if !ok || v == "" {
		return nil, errors.New("peer public key is unavailable")
	}
	return base64.RawURLEncoding.DecodeString(v)
}

func (s *Service) productName() string {
	// Non-capability adapters report a neutral product; the response is
	// available:false either way.
	return "unknown"
}

// ── Signatures ─────────────────────────────────────────────────────

func (s *Service) signQuery(req protocol.CapabilityQuery) string {
	return identity.Sign(s.id.PrivateKey, []byte(s.querySignatureInput(req)))
}

func (s *Service) signResponse(resp protocol.CapabilityResponse) string {
	return identity.Sign(s.id.PrivateKey, []byte(s.responseSignatureInput(resp)))
}

func (s *Service) signSet(req protocol.CapabilitySet) string {
	return identity.Sign(s.id.PrivateKey, []byte(s.setSignatureInput(req)))
}

func (s *Service) signSetResponse(resp protocol.CapabilitySetResponse) string {
	return identity.Sign(s.id.PrivateKey, []byte(s.setResponseSignatureInput(resp)))
}

func (s *Service) signExecutionsQuery(req protocol.CapabilityExecutionsQuery) string {
	return identity.Sign(s.id.PrivateKey, []byte(s.executionsQuerySignatureInput(req)))
}

func (s *Service) signExecutionsResponse(resp protocol.CapabilityExecutionsResponse) string {
	return identity.Sign(s.id.PrivateKey, []byte(s.executionsResponseSignatureInput(resp)))
}

func (s *Service) querySignatureInput(req protocol.CapabilityQuery) string {
	return stringsJoin(req.MessageType, req.From, req.To, req.RequestID, fmt.Sprintf("%d", req.Ts))
}

func (s *Service) responseSignatureInput(resp protocol.CapabilityResponse) string {
	return stringsJoin(resp.MessageType, resp.From, resp.To, resp.RequestID,
		fmt.Sprintf("%t", resp.Available), resp.Reason, fmt.Sprintf("%d", resp.Ts))
}

func (s *Service) setSignatureInput(req protocol.CapabilitySet) string {
	return stringsJoin(req.MessageType, req.From, req.To, req.RequestID,
		req.Target, req.Action, req.Skill, req.Model, fmt.Sprintf("%d", req.Ts))
}

func (s *Service) setResponseSignatureInput(resp protocol.CapabilitySetResponse) string {
	return stringsJoin(resp.MessageType, resp.From, resp.To, resp.RequestID,
		fmt.Sprintf("%t", resp.OK), resp.Target, resp.Action, fmt.Sprintf("%d", resp.Ts))
}

func (s *Service) executionsQuerySignatureInput(req protocol.CapabilityExecutionsQuery) string {
	return stringsJoin(req.MessageType, req.From, req.To, req.RequestID,
		req.JobID, fmt.Sprintf("%d", req.Limit), fmt.Sprintf("%d", req.Ts))
}

func (s *Service) executionsResponseSignatureInput(resp protocol.CapabilityExecutionsResponse) string {
	return stringsJoin(resp.MessageType, resp.From, resp.To, resp.RequestID,
		fmt.Sprintf("%d", len(resp.Executions)), resp.Error, fmt.Sprintf("%d", resp.Ts))
}

// ── Audit log ──────────────────────────────────────────────────────

func (s *Service) logAudit(req protocol.CapabilitySet) {
	if s.log == nil {
		return
	}
	s.log.Info("capability.set received",
		slog.String("sender", req.From),
		slog.String("target", req.Target),
		slog.String("action", req.Action),
		slog.String("skill", req.Skill),
		slog.String("model", req.Model),
		slog.String("jobId", req.JobID),
		slog.Any("fileIds", req.FileIDs),
		slog.String("ts", fmt.Sprintf("%d", req.Ts)),
	)
}

// ── Misc ───────────────────────────────────────────────────────────

func randID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func stringsJoin(parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}
