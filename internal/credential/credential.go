// Package credential resolves model API credentials without coupling command
// execution or environment precedence to the CLI bootstrap.
package credential

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultCommandTimeout = 5 * time.Second
	defaultStdoutLimit    = 16 * 1024
	defaultStderrLimit    = 4 * 1024
)

// Source describes the user-configurable credential sources. Environment
// lookup remains a Resolver dependency so tests and embedded clients do not
// mutate process-global state.
type Source struct {
	Token   string
	Command []string
}

// Resolver applies environment > command > static-token precedence.
type Resolver struct {
	LookupEnv      func(string) (string, bool)
	CommandTimeout time.Duration
	StdoutLimit    int
	StderrLimit    int
}

func (r Resolver) Resolve(ctx context.Context, source Source) (string, error) {
	lookupEnv := r.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if value, ok := lookupEnv("OPENAI_API_KEY"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value), nil
	}
	if len(source.Command) == 0 {
		return strings.TrimSpace(source.Token), nil
	}
	if strings.TrimSpace(source.Command[0]) == "" {
		return "", errors.New("token_command executable is empty")
	}
	timeout := r.CommandTimeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	stdoutLimit := r.StdoutLimit
	if stdoutLimit <= 0 {
		stdoutLimit = defaultStdoutLimit
	}
	stderrLimit := r.StderrLimit
	if stderrLimit <= 0 {
		stderrLimit = defaultStderrLimit
	}

	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandContext, source.Command[0], source.Command[1:]...)
	stdout := boundedWriter{limit: stdoutLimit}
	stderr := boundedWriter{limit: stderrLimit}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return "", fmt.Errorf("token_command failed: %w: %s", err, message)
		}
		return "", fmt.Errorf("token_command failed: %w", err)
	}
	key := strings.TrimSpace(stdout.String())
	if key == "" {
		return "", errors.New("token_command returned an empty token")
	}
	return key, nil
}

type boundedWriter struct {
	builder strings.Builder
	limit   int
}

func (w *boundedWriter) Write(value []byte) (int, error) {
	written := len(value)
	remaining := min(w.limit-w.builder.Len(), len(value))
	if remaining > 0 {
		_, _ = w.builder.Write(value[:remaining])
	}
	return written, nil
}

func (w *boundedWriter) String() string { return w.builder.String() }
