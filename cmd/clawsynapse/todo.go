package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"clawsynapse/pkg/types"
)

// runTodo implements `clawsynapse todo <subcommand>`.
//
// todo ask — request user confirmation during task execution.
//
// This is a thin wrapper over `publish --type todo.ask` that assembles the
// protocol payload (question_id / question / options) so agents don't have to
// hand-roll JSON or guess publish flags. The TrustMesh backend turns the
// published todo.ask into a todo_ask_received event, which the frontend
// renders as the interactive confirmation UI. Publishing a plain
// task.comment instead does NOT trigger that UI — always use this command.
func runTodo(ctx context.Context, client localAPIClient, args []string) (types.APIResult, error) {
	if len(args) == 0 {
		return types.APIResult{}, fmt.Errorf("missing todo subcommand (available: ask)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printTodoHelp(os.Stderr)
		return types.APIResult{}, flag.ErrHelp
	}
	switch args[0] {
	case "ask":
		return runTodoAsk(ctx, client, args[1:])
	default:
		return types.APIResult{}, fmt.Errorf("unknown todo subcommand: %s (available: ask)", args[0])
	}
}

func runTodoAsk(ctx context.Context, client localAPIClient, args []string) (types.APIResult, error) {
	fs := flag.NewFlagSet("todo ask", flag.ContinueOnError)
	target := fs.String("target", "", "target node id (the TrustMesh platform node)")
	taskID := fs.String("task-id", "", "task id (optional; resolved from session-key when omitted)")
	todoID := fs.String("todo", "", "todo id, e.g. TD_01")
	questionID := fs.String("question-id", "", "question id (auto-generated q_<unix_ms> when omitted)")
	question := fs.String("question", "", "question text, or @/path/to/file to read from a file")
	options := fs.String("options", "", "comma-separated options, e.g. A,B,C (optional)")
	sessionKey := fs.String("session-key", "", "session key (usually the task id)")
	var metadataFlags stringList
	fs.Var(&metadataFlags, "metadata", "metadata key=value; repeatable")
	if err := fs.Parse(args); err != nil {
		return types.APIResult{}, err
	}
	if strings.TrimSpace(*target) == "" {
		return types.APIResult{}, fmt.Errorf("missing --target")
	}
	if strings.TrimSpace(*todoID) == "" {
		return types.APIResult{}, fmt.Errorf("missing --todo")
	}
	if strings.TrimSpace(*question) == "" {
		return types.APIResult{}, fmt.Errorf("missing --question")
	}
	if strings.HasPrefix(*question, "@") {
		path := strings.TrimPrefix(*question, "@")
		if strings.TrimSpace(path) == "" {
			return types.APIResult{}, fmt.Errorf("--question @file: missing file path")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return types.APIResult{}, fmt.Errorf("--question @file: read %s: %w", path, err)
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return types.APIResult{}, fmt.Errorf("--question @file: %s is empty", path)
		}
		*question = string(raw)
	}
	qid := strings.TrimSpace(*questionID)
	if qid == "" {
		qid = fmt.Sprintf("q_%d", time.Now().UnixMilli())
	}
	payload := map[string]any{
		"todo_id":     *todoID,
		"question_id": qid,
		"question":    *question,
	}
	if strings.TrimSpace(*taskID) != "" {
		payload["task_id"] = strings.TrimSpace(*taskID)
	}
	if opts := strings.Split(*options, ","); strings.TrimSpace(*options) != "" {
		cleaned := make([]string, 0, len(opts))
		for _, o := range opts {
			if t := strings.TrimSpace(o); t != "" {
				cleaned = append(cleaned, t)
			}
		}
		if len(cleaned) > 0 {
			payload["options"] = cleaned
		}
	}
	msg, err := json.Marshal(payload)
	if err != nil {
		return types.APIResult{}, fmt.Errorf("encode payload: %w", err)
	}
	metadata, err := parseMetadata(metadataFlags)
	if err != nil {
		return types.APIResult{}, err
	}
	body := map[string]any{
		"targetNode": *target,
		"type":       "todo.ask",
		"message":    string(msg),
		"sessionKey": *sessionKey,
		"metadata":   metadata,
	}
	return client.Post(ctx, "/v1/publish", body)
}

func printTodoHelp(stderr *os.File) {
	fmt.Fprintln(stderr, "usage: clawsynapse todo ask [flags]")
	fmt.Fprintln(stderr, "")
	fmt.Fprintln(stderr, "Ask the user for confirmation during task execution (todo.ask protocol).")
	fmt.Fprintln(stderr, "Do NOT fall back to task.comment — plain comments never render the")
	fmt.Fprintln(stderr, "interactive confirmation UI on the platform.")
	fmt.Fprintln(stderr, "")
	fmt.Fprintln(stderr, "Flags:")
	fs := flag.NewFlagSet("todo ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.String("target", "", "target node id (the TrustMesh platform node)")
	fs.String("task-id", "", "task id (optional; resolved from session-key when omitted)")
	fs.String("todo", "", "todo id, e.g. TD_01")
	fs.String("question-id", "", "question id (auto-generated when omitted)")
	fs.String("question", "", "question text, or @/path/to/file to read from a file")
	fs.String("options", "", "comma-separated options, e.g. A,B,C (optional)")
	fs.String("session-key", "", "session key (usually the task id)")
	fs.Var(&stringList{}, "metadata", "metadata key=value; repeatable")
	fs.PrintDefaults()
	fmt.Fprintln(stderr, "")
	fmt.Fprintln(stderr, "Examples:")
	fmt.Fprintln(stderr, `  clawsynapse todo ask --target <node-id> --session-key <task-id> \`)
	fmt.Fprintln(stderr, `    --todo TD_01 --options "同意流转,需要调整" \`)
	fmt.Fprintln(stderr, `    --question "@/tmp/ask.txt"`)
}
