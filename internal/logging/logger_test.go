package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewCreatesParentDirectoryForLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "clawsynapsed.log")

	log, err := New(Options{
		FilePath: path,
		Rotate: RotateOptions{
			MaxSizeMB:  10,
			MaxBackups: 2,
			MaxAgeDays: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	log.Info("hello")

	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("expected log directory to exist: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}
}
