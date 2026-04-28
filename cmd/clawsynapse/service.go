package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const (
	serviceUnitName = "clawsynapsed.service"
	launchdLabel    = "io.github.yuanjun5681.clawsynapse.clawsynapsed"
)

var (
	serviceGOOS        = runtime.GOOS
	serviceUserHomeDir = os.UserHomeDir
	serviceGetUID      = os.Getuid
)

type serviceRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execServiceRunner struct{}

func (execServiceRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func runService(args []string, stdout, stderr io.Writer) error {
	return runServiceWithRunner(args, stdout, stderr, execServiceRunner{})
}

func runServiceWithRunner(args []string, stdout, stderr io.Writer, runner serviceRunner) error {
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		printServiceHelp(stderr)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		printServiceHelp(stderr)
		return flag.ErrHelp
	}

	action := strings.ToLower(strings.TrimSpace(rest[0]))
	if action == "help" || action == "-h" {
		printServiceHelp(stderr)
		return flag.ErrHelp
	}
	if action != "status" && action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("unknown service action: %s", rest[0])
	}
	if len(rest) > 1 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(rest[1:], " "))
	}

	ctx := context.Background()
	spec, err := buildServiceSpec(action)
	if err != nil {
		return err
	}
	if serviceGOOS == "darwin" {
		return runLaunchdAction(ctx, runner, action, stdout, stderr)
	}

	for i, cmd := range spec.commands {
		out, runErr := runner.Run(ctx, cmd.name, cmd.args...)
		if len(out) > 0 {
			target := stdout
			if runErr != nil {
				target = stderr
			}
			fmt.Fprint(target, string(out))
			if len(out) > 0 && out[len(out)-1] != '\n' {
				fmt.Fprintln(target)
			}
		}
		if runErr != nil {
			return fmt.Errorf("%s %s: %w", cmd.name, strings.Join(cmd.args, " "), runErr)
		}
		if i == len(spec.commands)-1 && spec.success != "" {
			fmt.Fprintln(stdout, spec.success)
		}
	}

	return nil
}

type serviceCommand struct {
	name string
	args []string
}

type serviceSpec struct {
	commands []serviceCommand
	success  string
}

func buildServiceSpec(action string) (serviceSpec, error) {
	switch serviceGOOS {
	case "linux":
		return buildSystemdSpec(action), nil
	case "darwin":
		return serviceSpec{}, nil
	default:
		return serviceSpec{}, fmt.Errorf("service command is not supported on %s", serviceGOOS)
	}
}

func buildSystemdSpec(action string) serviceSpec {
	switch action {
	case "status":
		return serviceSpec{
			commands: []serviceCommand{
				{name: "sudo", args: []string{"systemctl", "status", serviceUnitName, "--no-pager"}},
			},
		}
	case "start":
		return serviceSpec{
			commands: []serviceCommand{
				{name: "sudo", args: []string{"systemctl", "start", serviceUnitName}},
			},
			success: "service started",
		}
	case "stop":
		return serviceSpec{
			commands: []serviceCommand{
				{name: "sudo", args: []string{"systemctl", "stop", serviceUnitName}},
			},
			success: "service stopped",
		}
	case "restart":
		return serviceSpec{
			commands: []serviceCommand{
				{name: "sudo", args: []string{"systemctl", "restart", serviceUnitName}},
			},
			success: "service restarted",
		}
	default:
		return serviceSpec{}
	}
}

func buildLaunchdSpec(action, domain, home string) (serviceSpec, error) {
	plistPath := home + "/Library/LaunchAgents/" + launchdLabel + ".plist"
	target := domain + "/" + launchdLabel

	switch action {
	case "status":
		return serviceSpec{
			commands: []serviceCommand{
				{name: "launchctl", args: []string{"print", target}},
			},
		}, nil
	case "stop":
		return serviceSpec{
			commands: []serviceCommand{
				{name: "launchctl", args: []string{"bootout", target}},
			},
			success: "service stopped",
		}, nil
	default:
		return serviceSpec{
			commands: []serviceCommand{
				{name: "launchctl", args: []string{"bootstrap", domain, plistPath}},
				{name: "launchctl", args: []string{"enable", target}},
				{name: "launchctl", args: []string{"kickstart", "-k", target}},
			},
			success: "service started",
		}, nil
	}
}

func runLaunchdAction(ctx context.Context, runner serviceRunner, action string, stdout, stderr io.Writer) error {
	domain, err := resolveLaunchdDomain(ctx, runner)
	if err != nil {
		return err
	}
	home, err := serviceUserHomeDir()
	if err != nil {
		return err
	}
	spec, err := buildLaunchdSpec(action, domain, home)
	if err != nil {
		return err
	}

	if action == "start" || action == "restart" {
		target := domain + "/" + launchdLabel
		if out, runErr := runner.Run(ctx, "launchctl", "kickstart", "-k", target); runErr == nil {
			if len(out) > 0 {
				fmt.Fprint(stdout, string(out))
				if out[len(out)-1] != '\n' {
					fmt.Fprintln(stdout)
				}
			}
			if action == "start" {
				fmt.Fprintln(stdout, "service started")
			} else {
				fmt.Fprintln(stdout, "service restarted")
			}
			return nil
		}
		if action == "restart" {
			_, _ = runner.Run(ctx, "launchctl", "bootout", target)
		}
	}

	for i, cmd := range spec.commands {
		out, runErr := runner.Run(ctx, cmd.name, cmd.args...)
		if len(out) > 0 {
			target := stdout
			if runErr != nil {
				target = stderr
			}
			fmt.Fprint(target, string(out))
			if out[len(out)-1] != '\n' {
				fmt.Fprintln(target)
			}
		}
		if runErr != nil {
			return fmt.Errorf("%s %s: %w", cmd.name, strings.Join(cmd.args, " "), runErr)
		}
		if i == len(spec.commands)-1 {
			if action == "restart" {
				fmt.Fprintln(stdout, "service restarted")
			} else if spec.success != "" {
				fmt.Fprintln(stdout, spec.success)
			}
		}
	}
	return nil
}

func resolveLaunchdDomain(ctx context.Context, runner serviceRunner) (string, error) {
	uid := strconv.Itoa(serviceGetUID())
	guiDomain := "gui/" + uid
	if _, err := runner.Run(ctx, "launchctl", "print", guiDomain); err == nil {
		return guiDomain, nil
	}
	return "user/" + uid, nil
}

func printServiceHelp(stderr io.Writer) {
	fmt.Fprintln(stderr, "usage: clawsynapse service <action>")
	fmt.Fprintln(stderr, "")
	fmt.Fprintln(stderr, "Actions:")
	fmt.Fprintln(stderr, "  status    show daemon service status")
	fmt.Fprintln(stderr, "  start     start the daemon service")
	fmt.Fprintln(stderr, "  stop      stop the daemon service")
	fmt.Fprintln(stderr, "  restart   restart the daemon service")
	fmt.Fprintln(stderr, "")
	fmt.Fprintln(stderr, "Examples:")
	fmt.Fprintln(stderr, "  clawsynapse service status")
	fmt.Fprintln(stderr, "  clawsynapse service restart")
}
