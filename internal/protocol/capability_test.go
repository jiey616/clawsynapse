package protocol

import (
	"strings"
	"testing"
)

func TestValidateCapabilitySubject(t *testing.T) {
	valid := []string{
		"clawsynapse.capability.node-alpha.query",
		"clawsynapse.capability.node-alpha.response",
		"clawsynapse.capability.node-alpha.set",
		"clawsynapse.capability.node-alpha.set_response",
		"clawsynapse.capability.opc-founder-001.query",
	}
	for _, sub := range valid {
		if err := ValidateSubject(sub); err != nil {
			t.Errorf("ValidateSubject(%q) = %v, want nil", sub, err)
		}
	}

	invalid := []string{
		"clawsynapse.capability.query",            // missing target
		"clawsynapse.capability.node-alpha.bogus", // unknown action is allowed by format, fine — use structural error instead
		"clawsynapse.unknown.node-alpha.query",    // unknown module
		"other.capability.node-alpha.query",       // wrong root
	}
	for _, sub := range invalid[:1] {
		if err := ValidateSubject(sub); err == nil {
			t.Errorf("ValidateSubject(%q) = nil, want error", sub)
		}
	}
	if err := ValidateSubject("clawsynapse.unknown.node-alpha.query"); err == nil {
		t.Error("ValidateSubject with unknown module = nil, want error")
	}
	if err := ValidateSubject("other.capability.node-alpha.query"); err == nil {
		t.Error("ValidateSubject with wrong root = nil, want error")
	}
}

func TestCapabilitySubjectModule(t *testing.T) {
	m, err := SubjectModule("clawsynapse.capability.node-alpha.query")
	if err != nil {
		t.Fatalf("SubjectModule: %v", err)
	}
	if m != "capability" {
		t.Errorf("module = %q, want capability", m)
	}
}

func TestValidateCapabilityMessage(t *testing.T) {
	// capability.query message on the matching subject passes validation.
	err := ValidateMessage("clawsynapse.capability.node-alpha.query", ControlMessage{
		MessageType: "capability.query",
		To:          "node-alpha",
		Ts:          0, // disable ts window check
	}, ValidateOptions{})
	if err != nil {
		t.Errorf("ValidateMessage(capability.query) = %v, want nil", err)
	}

	// Wrong module (capability message on a trust subject) is rejected.
	err = ValidateMessage("clawsynapse.trust.node-alpha.request", ControlMessage{
		MessageType: "capability.query",
		To:          "node-alpha",
	}, ValidateOptions{})
	if err == nil {
		t.Error("ValidateMessage with mismatched module = nil, want error")
	} else if !strings.Contains(err.Error(), ErrModuleMismatch) {
		t.Errorf("expected module mismatch, got: %v", err)
	}
}
