package transfer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBucketName(t *testing.T) {
	want := "clawsynapse-transfer-node-alpha"
	got := bucketName("node-alpha")
	if got != want {
		t.Fatalf("bucketName(node-alpha) = %q, want %q", got, want)
	}
}

func TestParseTTL(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"", 24 * time.Hour},
		{"12h", 12 * time.Hour},
		{"30m", 30 * time.Minute},
		{"invalid", 24 * time.Hour},
	}
	for _, tt := range tests {
		got := parseTTL(tt.input)
		if got != tt.want {
			t.Fatalf("parseTTL(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestNewTransferID(t *testing.T) {
	id := newTransferID()
	if id == "" {
		t.Fatal("newTransferID returned empty string")
	}
	id2 := newTransferID()
	if id == id2 {
		t.Fatal("newTransferID returned duplicate IDs")
	}
}

func TestToTransferInfo(t *testing.T) {
	rec := &TransferRecord{
		TransferID:  "tf-1",
		Direction:   "inbound",
		PeerNode:    "node-beta",
		FileName:    "test.txt",
		FileSize:    1024,
		MimeType:    "text/plain",
		Checksum:    "sha256-abc",
		Status:      "completed",
		LocalPath:   "/tmp/transfers/tf-1-test.txt",
		Metadata:    map[string]any{"taskId": "task-001"},
		CreatedAt:   1000,
		CompletedAt: 2000,
	}
	info := toTransferInfo(rec)
	if info.TransferID != rec.TransferID {
		t.Fatalf("TransferID = %q, want %q", info.TransferID, rec.TransferID)
	}
	if info.Direction != rec.Direction {
		t.Fatalf("Direction = %q, want %q", info.Direction, rec.Direction)
	}
	if info.PeerNode != rec.PeerNode {
		t.Fatalf("PeerNode = %q, want %q", info.PeerNode, rec.PeerNode)
	}
	if info.FileName != rec.FileName {
		t.Fatalf("FileName = %q, want %q", info.FileName, rec.FileName)
	}
	if info.FileSize != rec.FileSize {
		t.Fatalf("FileSize = %d, want %d", info.FileSize, rec.FileSize)
	}
	if info.LocalPath != rec.LocalPath {
		t.Fatalf("LocalPath = %q, want %q", info.LocalPath, rec.LocalPath)
	}
	if info.Metadata == nil {
		t.Fatal("Metadata is nil, want non-nil")
	}
	if info.Metadata["taskId"] != "task-001" {
		t.Fatalf("Metadata[taskId] = %v, want task-001", info.Metadata["taskId"])
	}
}

func TestIsAlreadyDownloaded(t *testing.T) {
	svc := &Service{
		transfers: map[string]*TransferRecord{
			"tf-done": {
				TransferID: "tf-done",
				Direction:  "inbound",
				Status:     "completed",
			},
			"tf-outbound": {
				TransferID: "tf-outbound",
				Direction:  "outbound",
				Status:     "completed",
			},
			"tf-pending": {
				TransferID: "tf-pending",
				Direction:  "inbound",
				Status:     "pending",
			},
		},
	}

	if !svc.isAlreadyDownloaded("tf-done") {
		t.Fatal("expected tf-done to be already downloaded")
	}
	if svc.isAlreadyDownloaded("tf-outbound") {
		t.Fatal("outbound transfer should not count as downloaded")
	}
	if svc.isAlreadyDownloaded("tf-pending") {
		t.Fatal("pending transfer should not count as downloaded")
	}
	if svc.isAlreadyDownloaded("tf-unknown") {
		t.Fatal("unknown transfer should not count as downloaded")
	}
}

func TestListTransfers(t *testing.T) {
	svc := &Service{
		transfers: map[string]*TransferRecord{
			"tf-1": {TransferID: "tf-1", Direction: "inbound", Status: "completed"},
			"tf-2": {TransferID: "tf-2", Direction: "outbound", Status: "completed"},
		},
	}

	list := svc.ListTransfers()
	if len(list) != 2 {
		t.Fatalf("ListTransfers() returned %d items, want 2", len(list))
	}
}

func TestGetTransfer(t *testing.T) {
	svc := &Service{
		transfers: map[string]*TransferRecord{
			"tf-1": {TransferID: "tf-1", Direction: "inbound", Status: "completed"},
		},
	}

	info, ok := svc.GetTransfer("tf-1")
	if !ok {
		t.Fatal("expected to find tf-1")
	}
	if info.TransferID != "tf-1" {
		t.Fatalf("TransferID = %q, want tf-1", info.TransferID)
	}

	_, ok = svc.GetTransfer("tf-missing")
	if ok {
		t.Fatal("expected not to find tf-missing")
	}
}

func TestOnReceivedCallback(t *testing.T) {
	called := make(chan TransferRecord, 1)
	svc := &Service{
		transfers: make(map[string]*TransferRecord),
	}
	svc.OnReceived(func(rec TransferRecord) {
		called <- rec
	})

	// Simulate what pullAndSave does after file download
	rec := &TransferRecord{
		TransferID:  "tf-cb",
		Direction:   "inbound",
		PeerNode:    "node-gamma",
		FileName:    "data.csv",
		FileSize:    512,
		MimeType:    "text/csv",
		Status:      "completed",
		LocalPath:   "/tmp/transfers/tf-cb-data.csv",
		Metadata:    map[string]any{"todoId": "todo-99"},
		CreatedAt:   1000,
		CompletedAt: 2000,
	}
	svc.mu.Lock()
	svc.transfers[rec.TransferID] = rec
	svc.mu.Unlock()

	svc.mu.Lock()
	fn := svc.onReceived
	svc.mu.Unlock()
	if fn != nil {
		go fn(*rec)
	}

	select {
	case got := <-called:
		if got.TransferID != "tf-cb" {
			t.Fatalf("TransferID = %q, want tf-cb", got.TransferID)
		}
		if got.FileName != "data.csv" {
			t.Fatalf("FileName = %q, want data.csv", got.FileName)
		}
		if got.Metadata["todoId"] != "todo-99" {
			t.Fatalf("Metadata[todoId] = %v, want todo-99", got.Metadata["todoId"])
		}
	case <-time.After(time.Second):
		t.Fatal("onReceived callback not called within timeout")
	}
}

func TestOnReceivedNilDoesNotPanic(t *testing.T) {
	svc := &Service{
		transfers: make(map[string]*TransferRecord),
	}
	// onReceived is nil by default — simulate the guard in pullAndSave
	svc.mu.Lock()
	fn := svc.onReceived
	svc.mu.Unlock()
	if fn != nil {
		t.Fatal("expected onReceived to be nil")
	}
}

func TestTransferConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := TransferConfig{
		TransferDir: filepath.Join(dir, "transfers"),
		MaxFileSize: 5000,
		TTL:         "1h",
	}

	svc := &Service{
		transferDir: cfg.TransferDir,
		maxFileSize: cfg.MaxFileSize,
		ttl:         parseTTL(cfg.TTL),
		transfers:   make(map[string]*TransferRecord),
	}

	if svc.maxFileSize != 5000 {
		t.Fatalf("maxFileSize = %d, want 5000", svc.maxFileSize)
	}
	if svc.ttl != time.Hour {
		t.Fatalf("ttl = %v, want 1h", svc.ttl)
	}
}

func TestTransferDirCreation(t *testing.T) {
	dir := t.TempDir()
	transferDir := filepath.Join(dir, "nested", "transfers")

	if err := os.MkdirAll(transferDir, 0o700); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	info, err := os.Stat(transferDir)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}
