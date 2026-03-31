package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"clawsynapse/pkg/types"
)

func TestRunVersionPrintsVersion(t *testing.T) {
	stdout := tempFile(t)
	stderr := tempFile(t)

	oldVersion := version
	version = "v9.9.9"
	t.Cleanup(func() {
		version = oldVersion
	})

	code := run([]string{"version"}, stdout, stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}

	got := readFile(t, stdout)
	if got != "v9.9.9\n" {
		t.Fatalf("unexpected stdout %q", got)
	}
}

func TestRunTransferSendPostsExpectedPayload(t *testing.T) {
	client := &stubAPIClient{result: types.APIResult{OK: true, Code: "transfer.sent"}}
	_, err := runTransfer(context.Background(), client, []string{
		"send",
		"--target", "node-beta",
		"--file", "/tmp/demo.txt",
		"--mime-type", "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.method != "POST" {
		t.Fatalf("expected POST, got %s", client.method)
	}
	if client.endpoint != "/v1/transfer/send" {
		t.Fatalf("unexpected endpoint %s", client.endpoint)
	}
	body, _ := client.payload.(map[string]any)
	if body["targetNode"] != "node-beta" {
		t.Fatalf("unexpected targetNode %#v", body["targetNode"])
	}
	if body["filePath"] != "/tmp/demo.txt" {
		t.Fatalf("unexpected filePath %#v", body["filePath"])
	}
	if body["mimeType"] != "text/plain" {
		t.Fatalf("unexpected mimeType %#v", body["mimeType"])
	}
}

func TestRunTransferSendIncludesMetadata(t *testing.T) {
	client := &stubAPIClient{result: types.APIResult{OK: true, Code: "transfer.sent"}}
	_, err := runTransfer(context.Background(), client, []string{
		"send",
		"--target", "node-beta",
		"--file", "/tmp/demo.txt",
		"--metadata", "taskId=task-001",
		"--metadata", "todoId=todo-042",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := client.payload.(map[string]any)
	md, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatal("expected metadata in payload")
	}
	if md["taskId"] != "task-001" {
		t.Fatalf("metadata taskId = %v, want task-001", md["taskId"])
	}
	if md["todoId"] != "todo-042" {
		t.Fatalf("metadata todoId = %v, want todo-042", md["todoId"])
	}
}

func TestRunTransferSendOmitsEmptyMetadata(t *testing.T) {
	client := &stubAPIClient{result: types.APIResult{OK: true, Code: "transfer.sent"}}
	_, err := runTransfer(context.Background(), client, []string{
		"send",
		"--target", "node-beta",
		"--file", "/tmp/demo.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := client.payload.(map[string]any)
	if _, exists := body["metadata"]; exists {
		t.Fatal("expected no metadata key when --metadata not provided")
	}
}

func TestRunTransferGetFetchesTransferByID(t *testing.T) {
	client := &stubAPIClient{result: types.APIResult{OK: true, Code: "transfer.detail"}}
	_, err := runTransfer(context.Background(), client, []string{"get", "--id", "tr-123"})
	if err != nil {
		t.Fatal(err)
	}
	if client.method != "GET" {
		t.Fatalf("expected GET, got %s", client.method)
	}
	if client.endpoint != "/v1/transfer/tr-123" {
		t.Fatalf("unexpected endpoint %s", client.endpoint)
	}
}

func TestRunTransferDeleteCallsDeleteEndpoint(t *testing.T) {
	client := &stubAPIClient{result: types.APIResult{OK: true, Code: "transfer.deleted"}}
	_, err := runTransfer(context.Background(), client, []string{"delete", "--id", "tr-456"})
	if err != nil {
		t.Fatal(err)
	}
	if client.method != "DELETE" {
		t.Fatalf("expected DELETE, got %s", client.method)
	}
	if client.endpoint != "/v1/transfer/tr-456" {
		t.Fatalf("unexpected endpoint %s", client.endpoint)
	}
}

func TestRunTransferListUsesTransfersEndpoint(t *testing.T) {
	client := &stubAPIClient{result: types.APIResult{OK: true, Code: "transfer.list"}}
	_, err := runTransfer(context.Background(), client, []string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	if client.method != "GET" {
		t.Fatalf("expected GET, got %s", client.method)
	}
	if client.endpoint != "/v1/transfers" {
		t.Fatalf("unexpected endpoint %s", client.endpoint)
	}
}

type stubAPIClient struct {
	method   string
	endpoint string
	payload  any
	result   types.APIResult
	err      error
}

func (s *stubAPIClient) Get(_ context.Context, endpoint string) (types.APIResult, error) {
	s.method = "GET"
	s.endpoint = endpoint
	s.payload = nil
	return s.result, s.err
}

func (s *stubAPIClient) Post(_ context.Context, endpoint string, payload any) (types.APIResult, error) {
	s.method = "POST"
	s.endpoint = endpoint
	s.payload = payload
	return s.result, s.err
}

func (s *stubAPIClient) Delete(_ context.Context, endpoint string) (types.APIResult, error) {
	s.method = "DELETE"
	s.endpoint = endpoint
	s.payload = nil
	return s.result, s.err
}

func tempFile(t *testing.T) *os.File {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "clawsynapse-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.Close()
	})
	return f
}

func readFile(t *testing.T, f *os.File) string {
	t.Helper()

	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}
