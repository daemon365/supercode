package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/daemon365/supercode/internal/permission"
)

type SandboxOptions struct {
	ReadRoots   []string
	WriteRoots  []string
	DenyRoots   []string
	Permissions *permission.Manager
}

// commandSandbox provides a workspace-write boundary on Linux.
// The host filesystem is mounted read-only, the workspace is optionally
// reopened read-write and /tmp is ephemeral. The network namespace is also
// isolated where the host permits it. When bubblewrap is unavailable, every
// shell command remains an approval-requiring operation and runs without
// claiming sandbox protection.
type commandSandbox struct {
	workspace      workspace
	bwrap          string
	isolateNetwork bool
}

var bubblewrapProbe struct {
	sync.Once
	path           string
	isolateNetwork bool
}

func newCommandSandbox(workspace workspace) commandSandbox {
	value := commandSandbox{workspace: workspace}
	if runtime.GOOS != "linux" {
		return value
	}
	bubblewrapProbe.Do(func() {
		path, err := exec.LookPath("bwrap")
		if err != nil {
			return
		}
		path, err = filepath.Abs(path)
		if err != nil || !probeBubblewrap(path, false) {
			return
		}
		bubblewrapProbe.path = path
		bubblewrapProbe.isolateNetwork = probeBubblewrap(path, true)
	})
	if bubblewrapProbe.path == "" || within(workspace.root, bubblewrapProbe.path) {
		return value
	}
	value.bwrap = bubblewrapProbe.path
	value.isolateNetwork = bubblewrapProbe.isolateNetwork
	return value
}

func probeBubblewrap(path string, isolateNetwork bool) bool {
	arguments := []string{"--die-with-parent", "--new-session", "--unshare-user", "--unshare-pid"}
	if isolateNetwork {
		arguments = append(arguments, "--unshare-net")
	}
	arguments = append(arguments, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--", "/bin/true")
	return exec.Command(path, arguments...).Run() == nil
}

func (s commandSandbox) available() bool { return s.bwrap != "" }

func (s commandSandbox) status() string {
	if s.available() {
		if s.isolateNetwork {
			return "workspace-write (bubblewrap, network isolated)"
		}
		return "workspace-write (bubblewrap; network covered by approval policy)"
	}
	switch runtime.GOOS {
	case "darwin":
		return "approval-only fallback (macOS Seatbelt sandbox unavailable)"
	case "windows":
		return "approval-only fallback (Windows restricted sandbox unavailable)"
	default:
		return "approval-only fallback (bubblewrap unavailable)"
	}
}

func (s commandSandbox) command(ctx context.Context, shell string, shellArgs []string, workdir string, writable, escalated bool) *exec.Cmd {
	if escalated || !s.available() {
		command := exec.CommandContext(ctx, shell, shellArgs...)
		command.Dir = workdir
		return command
	}

	arguments := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-user",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	if s.isolateNetwork && (s.workspace.permissions == nil || !s.workspace.permissions.AllowsUnrestrictedNetwork()) {
		arguments = append([]string{"--unshare-net"}, arguments...)
	}
	if within(os.TempDir(), s.workspace.root) {
		var directories []string
		for directory := filepath.Dir(s.workspace.root); directory != os.TempDir() && directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
			directories = append(directories, directory)
		}
		for index := len(directories) - 1; index >= 0; index-- {
			arguments = append(arguments, "--dir", directories[index])
		}
	}
	if writable {
		for _, root := range s.workspace.allowedRoots(true) {
			arguments = append(arguments, "--bind", root, root)
		}
		for _, protected := range []string{".git", ".supercode"} {
			path := filepath.Join(s.workspace.root, protected)
			if _, err := os.Lstat(path); err == nil {
				arguments = append(arguments, "--ro-bind", path, path)
			}
		}
	} else {
		for _, root := range s.workspace.allowedRoots(false) {
			arguments = append(arguments, "--ro-bind", root, root)
		}
	}
	for _, denied := range s.workspace.denyRoots {
		info, err := os.Stat(denied)
		if err != nil {
			continue
		}
		if info.IsDir() {
			arguments = append(arguments, "--tmpfs", denied)
		} else {
			arguments = append(arguments, "--ro-bind", "/dev/null", denied)
		}
	}
	arguments = append(arguments, "--chdir", workdir, "--setenv", "TMPDIR", "/tmp", "--", shell)
	arguments = append(arguments, shellArgs...)
	return exec.CommandContext(ctx, s.bwrap, arguments...)
}

func (s commandSandbox) riskFor(command string, escalated bool) Risk {
	if escalated || !s.available() || !readOnlyShellCommand(command) {
		return RiskExecute
	}
	return RiskRead
}

// readOnlyShellCommand is deliberately conservative. Classification only
// removes the approval prompt because the command is still run inside a
// read-only bubblewrap sandbox. Commands that can initiate network activity
// are intentionally excluded when network namespaces are unavailable.
func readOnlyShellCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "\n;&|><`$(){}") {
		return false
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	program := filepath.Base(strings.Trim(fields[0], "'\""))
	switch program {
	case "pwd", "ls", "cat", "head", "tail", "wc", "file", "stat", "tree":
		return true
	case "rg":
		for _, field := range fields[1:] {
			if field == "--pre" || strings.HasPrefix(field, "--pre=") || field == "--pre-glob" || strings.HasPrefix(field, "--pre-glob=") {
				return false
			}
		}
		return true
	case "sed":
		if len(fields) < 3 {
			return false
		}
		programIndex := 1
		if fields[1] == "-n" || fields[1] == "--quiet" || fields[1] == "--silent" {
			programIndex++
		}
		if programIndex >= len(fields)-1 {
			return false
		}
		program := strings.Trim(fields[programIndex], "'\"")
		return validSedPrintProgram(program)
	}
	return false
}

func validSedPrintProgram(program string) bool {
	if !strings.HasSuffix(program, "p") {
		return false
	}
	program = strings.TrimSuffix(program, "p")
	if program == "" {
		return false
	}
	for _, character := range program {
		if (character < '0' || character > '9') && character != ',' && character != '$' {
			return false
		}
	}
	return true
}

func sandboxRequest(arguments string, commandField string) (command string, escalated bool) {
	var value map[string]any
	if decodeArguments(arguments, &value) != nil {
		return "", false
	}
	command, _ = value[commandField].(string)
	permissions, _ := value["sandbox_permissions"].(string)
	return command, permissions == "require-escalated"
}
