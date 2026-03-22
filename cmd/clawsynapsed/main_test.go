package main

import "testing"

func TestIsVersionCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "subcommand", args: []string{"version"}, want: true},
		{name: "flag", args: []string{"--version"}, want: true},
		{name: "empty", args: nil, want: false},
		{name: "extra args", args: []string{"--version", "--check-config"}, want: false},
		{name: "other", args: []string{"--check-config"}, want: false},
	}

	for _, tt := range tests {
		if got := isVersionCommand(tt.args); got != tt.want {
			t.Fatalf("%s: isVersionCommand(%v) = %v, want %v", tt.name, tt.args, got, tt.want)
		}
	}
}
