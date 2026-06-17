package types

import "testing"

func TestAPIResultFields(t *testing.T) {
	r := APIResult{OK: true, Code: "ok", TS: 0}
	if !r.OK {
		t.Error("expected OK true")
	}
	if r.Code != "ok" {
		t.Error("expected code ok")
	}
}
