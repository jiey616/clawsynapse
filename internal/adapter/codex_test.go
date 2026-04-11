package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clawsynapse/internal/store"
)

const codexSampleCreate = `{"type":"thread.started","thread_id":"019d6781-a2e1-7452-b624-7ac4c611151e"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"pong"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":1}}
`

func TestCodexAdapterDeliverMessageCreatesAndStoresSessionMapping(t *testing.T) {
	fs := store.NewFSStore(t.TempDir())
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}

	a, err := NewCodexAdapter(CodexConfig{
		NodeID:       "node-alpha",
		SessionStore: fs,
	})
	if err != nil {
		t.Fatalf("NewCodexAdapter failed: %v", err)
	}

	a.execCmd = func(_ context.Context, args ...string) ([]byte, error) {
		// 首次: exec --json --skip-git-repo-check -- <msg>
		if len(args) != 5 {
			t.Fatalf("unexpected args: %v", args)
		}
		if args[0] != "exec" || args[1] != "--json" || args[2] != "--skip-git-repo-check" || args[3] != "--" {
			t.Fatalf("unexpected prefix: %v", args[:4])
		}
		wantMsg := "[clawsynapse from=node-beta to=node-alpha session=session-1]\nhello"
		if args[4] != wantMsg {
			t.Fatalf("prompt = %q, want %q", args[4], wantMsg)
		}
		return []byte(codexSampleCreate), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := a.DeliverMessage(ctx, DeliverMessageRequest{
		SessionKey: "session-1",
		Message:    "hello",
		From:       "node-beta",
	})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !result.Success || !result.Accepted {
		t.Fatal("expected success+accepted")
	}
	if result.Reply != "pong" {
		t.Fatalf("reply = %q, want pong", result.Reply)
	}
	if result.SessionID != "019d6781-a2e1-7452-b624-7ac4c611151e" {
		t.Fatalf("sessionID = %q", result.SessionID)
	}

	st, ok, err := fs.LoadSessionState("codex", "session-1")
	if err != nil {
		t.Fatalf("LoadSessionState failed: %v", err)
	}
	if !ok {
		t.Fatal("expected mapping persisted")
	}
	if st.SessionID != "019d6781-a2e1-7452-b624-7ac4c611151e" {
		t.Fatalf("saved sessionID = %q", st.SessionID)
	}
}

func TestCodexAdapterDeliverMessageUsesMappedSessionID(t *testing.T) {
	fs := store.NewFSStore(t.TempDir())
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}
	if err := fs.SaveSessionState(store.SessionState{
		Adapter:     "codex",
		SessionKey:  "session-1",
		SessionID:   "thread-existing",
		CreatedAtMs: 1000,
		UpdatedAtMs: 1000,
	}); err != nil {
		t.Fatalf("SaveSessionState failed: %v", err)
	}

	a, _ := NewCodexAdapter(CodexConfig{NodeID: "node-1", SessionStore: fs})
	a.execCmd = func(_ context.Context, args ...string) ([]byte, error) {
		// 恢复: exec resume --json --skip-git-repo-check <id> -- <msg>
		if len(args) != 7 {
			t.Fatalf("unexpected args: %v", args)
		}
		if args[0] != "exec" || args[1] != "resume" {
			t.Fatalf("want exec resume, got %v", args[:2])
		}
		if args[4] != "thread-existing" {
			t.Fatalf("session id arg = %q", args[4])
		}
		if args[5] != "--" {
			t.Fatalf("want -- separator, got %q", args[5])
		}
		return []byte(`{"type":"thread.started","thread_id":"thread-existing"}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
`), nil
	}

	result, err := a.DeliverMessage(context.Background(), DeliverMessageRequest{
		SessionKey: "session-1",
		Message:    "hello",
		From:       "node-2",
	})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if result.SessionID != "thread-existing" {
		t.Fatalf("sessionID = %q", result.SessionID)
	}
	if result.Reply != "done" {
		t.Fatalf("reply = %q", result.Reply)
	}
}

func TestCodexAdapterRetriesUnknownMappedSession(t *testing.T) {
	fs := store.NewFSStore(t.TempDir())
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}
	if err := fs.SaveSessionState(store.SessionState{
		Adapter:    "codex",
		SessionKey: "session-1",
		SessionID:  "stale-thread",
	}); err != nil {
		t.Fatalf("SaveSessionState failed: %v", err)
	}

	a, _ := NewCodexAdapter(CodexConfig{NodeID: "node-alpha", SessionStore: fs})
	var calls int
	a.execCmd = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch calls {
		case 1:
			if args[1] != "resume" {
				t.Fatalf("first call should be resume, got %v", args)
			}
			return nil, errors.New("session not found: stale-thread")
		case 2:
			if len(args) != 5 || args[1] != "--json" {
				t.Fatalf("retry should be fresh exec, got %v", args)
			}
			return []byte(`{"type":"thread.started","thread_id":"fresh-thread"}
{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}
`), nil
		}
		t.Fatalf("unexpected call count %d", calls)
		return nil, nil
	}

	result, err := a.DeliverMessage(context.Background(), DeliverMessageRequest{
		SessionKey: "session-1",
		Message:    "hi",
		From:       "node-beta",
	})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if result.SessionID != "fresh-thread" {
		t.Fatalf("sessionID = %q", result.SessionID)
	}

	st, _, _ := fs.LoadSessionState("codex", "session-1")
	if st.SessionID != "fresh-thread" {
		t.Fatalf("saved sessionID = %q", st.SessionID)
	}
}

func TestCodexAdapterCommandFailure(t *testing.T) {
	a, _ := NewCodexAdapter(CodexConfig{})
	a.execCmd = func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, errors.New("command not found")
	}
	_, err := a.DeliverMessage(context.Background(), DeliverMessageRequest{Message: "hi"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCodexAdapterGetStatus(t *testing.T) {
	a, _ := NewCodexAdapter(CodexConfig{})
	a.execCmd = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) != 1 || args[0] != "--version" {
			t.Fatalf("unexpected args: %v", args)
		}
		return []byte("codex 0.40.0\n"), nil
	}
	status, err := a.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if !status.Healthy {
		t.Fatal("expected healthy")
	}
}

func TestParseCodexResultJSONL(t *testing.T) {
	result, err := parseCodexResult([]byte(codexSampleCreate))
	if err != nil {
		t.Fatalf("parseCodexResult failed: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if result.Reply != "pong" {
		t.Fatalf("reply = %q", result.Reply)
	}
	if result.SessionID != "019d6781-a2e1-7452-b624-7ac4c611151e" {
		t.Fatalf("sessionID = %q", result.SessionID)
	}
}

func TestParseCodexResultMultipleAgentMessagesTakesLast(t *testing.T) {
	data := []byte(`{"type":"thread.started","thread_id":"t1"}
{"type":"item.completed","item":{"type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"type":"agent_message","text":"final"}}
`)
	result, err := parseCodexResult(data)
	if err != nil {
		t.Fatalf("parseCodexResult failed: %v", err)
	}
	if result.Reply != "final" {
		t.Fatalf("reply = %q", result.Reply)
	}
}

func TestParseCodexResultDecodesEscapedNewlines(t *testing.T) {
	data := []byte(`{"type":"thread.started","thread_id":"t1"}
{"type":"item.completed","item":{"type":"agent_message","text":"line1\nline2"}}
`)
	result, err := parseCodexResult(data)
	if err != nil {
		t.Fatalf("parseCodexResult failed: %v", err)
	}
	if result.Reply != "line1\nline2" {
		t.Fatalf("reply = %q, want decoded multiline text", result.Reply)
	}
}

func TestParseCodexResultIgnoresNonAgentMessageItems(t *testing.T) {
	data := []byte(`{"type":"thread.started","thread_id":"t1"}
{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}
{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}
`)
	result, _ := parseCodexResult(data)
	if result.Reply != "answer" {
		t.Fatalf("reply = %q", result.Reply)
	}
}

func TestParseCodexResultTurnFailed(t *testing.T) {
	data := []byte(`{"type":"thread.started","thread_id":"t1"}
{"type":"turn.failed","message":"rate limited"}
`)
	result, _ := parseCodexResult(data)
	if result.Success {
		t.Fatal("expected failure")
	}
	if result.Error != "rate limited" {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestParseCodexResultEmpty(t *testing.T) {
	result, _ := parseCodexResult([]byte(""))
	if result.Success {
		t.Fatal("expected failure for empty output")
	}
}

func TestIsCodexUnknownSessionError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"session not found", true},
		{"no such session: abc", true},
		{"unknown session id", true},
		{"no recorded session matching --last", true},
		{"permission denied", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = errors.New(c.msg)
		}
		if got := isCodexUnknownSessionError(err); got != c.want {
			t.Errorf("isCodexUnknownSessionError(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestFormatCodexCommandForLogTruncatesPrompt(t *testing.T) {
	prompt := strings.Repeat("x", 400)
	got := formatCodexCommandForLog([]string{"exec", "--json", "--skip-git-repo-check", "--", prompt})
	if !strings.Contains(got, `codex "exec" "--json"`) {
		t.Fatalf("command = %q, missing prefix", got)
	}
	if !strings.Contains(got, "truncated, 400 bytes total") {
		t.Fatalf("command = %q, missing truncation marker", got)
	}
}

func TestRedactedCodexSessionIDResume(t *testing.T) {
	args := []string{"exec", "resume", "--json", "--skip-git-repo-check", "thread-xyz", "--", "hello"}
	if got := redactedCodexSessionID(args); got != "thread-xyz" {
		t.Fatalf("got %q, want thread-xyz", got)
	}
}

func TestRedactedCodexSessionIDFreshExec(t *testing.T) {
	args := []string{"exec", "--json", "--skip-git-repo-check", "--", "hello"}
	if got := redactedCodexSessionID(args); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
