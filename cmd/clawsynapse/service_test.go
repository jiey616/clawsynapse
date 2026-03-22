package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type serviceCall struct {
	name string
	args []string
}

type stubServiceRunner struct {
	calls     []serviceCall
	responses map[string]stubServiceResult
	queues    map[string][]stubServiceResult
}

type stubServiceResult struct {
	out []byte
	err error
}

func (s *stubServiceRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, serviceCall{name: name, args: append([]string(nil), args...)})
	key := name + " " + joinArgs(args)
	if queue, ok := s.queues[key]; ok && len(queue) > 0 {
		result := queue[0]
		s.queues[key] = queue[1:]
		return result.out, result.err
	}
	if result, ok := s.responses[key]; ok {
		return result.out, result.err
	}
	return nil, nil
}

func TestRunServiceLinuxRestart(t *testing.T) {
	prevGOOS := serviceGOOS
	serviceGOOS = "linux"
	defer func() { serviceGOOS = prevGOOS }()

	runner := &stubServiceRunner{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := runServiceWithRunner([]string{"restart"}, &stdout, &stderr, runner)
	if err != nil {
		t.Fatal(err)
	}

	want := []serviceCall{
		{name: "sudo", args: []string{"systemctl", "restart", serviceUnitName}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("unexpected calls: %#v", runner.calls)
	}
	if stdout.String() != "service restarted\n" {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
}

func TestRunServiceLaunchdRestartFallsBackToBootstrap(t *testing.T) {
	prevGOOS := serviceGOOS
	prevUID := serviceGetUID
	prevHome := serviceUserHomeDir
	serviceGOOS = "darwin"
	serviceGetUID = func() int { return 501 }
	serviceUserHomeDir = func() (string, error) { return "/Users/tester", nil }
	defer func() {
		serviceGOOS = prevGOOS
		serviceGetUID = prevUID
		serviceUserHomeDir = prevHome
	}()

	runner := &stubServiceRunner{
		queues: map[string][]stubServiceResult{
			"launchctl print gui/501": {
				{out: []byte("gui ok\n")},
			},
			"launchctl kickstart -k gui/501/" + launchdLabel: {
				{err: errors.New("service not loaded")},
				{},
			},
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runServiceWithRunner([]string{"restart"}, &stdout, &stderr, runner)
	if err != nil {
		t.Fatal(err)
	}

	want := []serviceCall{
		{name: "launchctl", args: []string{"print", "gui/501"}},
		{name: "launchctl", args: []string{"kickstart", "-k", "gui/501/" + launchdLabel}},
		{name: "launchctl", args: []string{"bootout", "gui/501/" + launchdLabel}},
		{name: "launchctl", args: []string{"bootstrap", "gui/501", "/Users/tester/Library/LaunchAgents/" + launchdLabel + ".plist"}},
		{name: "launchctl", args: []string{"enable", "gui/501/" + launchdLabel}},
		{name: "launchctl", args: []string{"kickstart", "-k", "gui/501/" + launchdLabel}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("unexpected calls: %#v", runner.calls)
	}
	if stdout.String() != "service restarted\n" {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
}

func TestRunServiceLaunchdStatusUsesUserDomainFallback(t *testing.T) {
	prevGOOS := serviceGOOS
	prevUID := serviceGetUID
	prevHome := serviceUserHomeDir
	serviceGOOS = "darwin"
	serviceGetUID = func() int { return 502 }
	serviceUserHomeDir = func() (string, error) { return os.TempDir(), nil }
	defer func() {
		serviceGOOS = prevGOOS
		serviceGetUID = prevUID
		serviceUserHomeDir = prevHome
	}()

	runner := &stubServiceRunner{
		responses: map[string]stubServiceResult{
			"launchctl print gui/502": {
				err: errors.New("not available"),
			},
			"launchctl print user/502/" + launchdLabel: {
				out: []byte("service state\n"),
			},
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runServiceWithRunner([]string{"status"}, &stdout, &stderr, runner)
	if err != nil {
		t.Fatal(err)
	}

	want := []serviceCall{
		{name: "launchctl", args: []string{"print", "gui/502"}},
		{name: "launchctl", args: []string{"print", "user/502/" + launchdLabel}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("unexpected calls: %#v", runner.calls)
	}
	if stdout.String() != "service state\n" {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
}

func joinArgs(args []string) string {
	return strings.Join(args, " ")
}
