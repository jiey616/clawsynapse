package adapter

import "testing"

// Compile-time check: HermesAdapter satisfies the optional stats interface.
var _ TaskStatsProvider = (*HermesAdapter)(nil)

func TestTaskCoordinatorTaskStatsEmpty(t *testing.T) {
	tc := NewTaskCoordinator(TaskConfig{}, nil, nil)
	st := tc.TaskStats()
	if !st.Available {
		t.Error("Available = false, want true for a live coordinator")
	}
	if st.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0", st.InFlight)
	}
	if st.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0", st.QueueDepth)
	}
	if st.MaxConcurrent <= 0 {
		t.Errorf("MaxConcurrent = %d, want normalized default > 0", st.MaxConcurrent)
	}
	if st.AdmittedTotal != 0 {
		t.Errorf("AdmittedTotal = %d, want 0", st.AdmittedTotal)
	}
}

func TestHermesAdapterTaskStatsNilCoordinator(t *testing.T) {
	a := &HermesAdapter{}
	if st := a.TaskStats(); st.Available {
		t.Error("Available = true, want false when task coordinator is nil")
	}
}
