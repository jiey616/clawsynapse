package obs

import (
	"bytes"
	"strings"
	"testing"
)

func TestCounterIncAddSnapshot(t *testing.T) {
	before := CounterSnapshot()["clawsynapse_test_total"]
	CounterInc("clawsynapse_test_total")
	CounterAdd("clawsynapse_test_total", 4)
	after := CounterSnapshot()["clawsynapse_test_total"]
	if after-before != 5 {
		t.Errorf("counter delta = %d, want 5", after-before)
	}
}

func TestWritePrometheus(t *testing.T) {
	CounterInc("clawsynapse_write_probe_total")
	var buf bytes.Buffer
	WritePrometheus(&buf)
	out := buf.String()
	if !strings.Contains(out, "# TYPE clawsynapse_write_probe_total counter") {
		t.Errorf("missing TYPE line for write probe, got %q", out)
	}
	if !strings.Contains(out, "clawsynapse_write_probe_total 1") {
		t.Errorf("missing counter value line, got %q", out)
	}
}
