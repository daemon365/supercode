package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/daemon365/supercode/internal/policy"
)

// Run resolves one CLI invocation and dispatches it to management, TUI, chat,
// or single-turn execution. Process signals and exit codes remain owned by the
// root main package.
func Run(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	lookupEnv func(string) (string, bool),
) error {
	startup, handled, err := prepareStartup(args, stdout, stderr, lookupEnv)
	if err != nil || handled {
		return err
	}
	configDirectory := filepath.Dir(startup.configPath)
	eventLogger, err := newEventLogger(startup.options)
	if err != nil {
		return err
	}
	policyStore, err := policy.NewStore(filepath.Join(configDirectory, "policy.yaml"))
	if err != nil {
		return err
	}
	if handled, err := runManagementCommand(ctx, startup, configDirectory, policyStore, stdout, lookupEnv); err != nil || handled {
		return err
	}
	environment, err := discoverProjectEnvironment(startup, configDirectory, policyStore)
	if err != nil {
		return err
	}
	if environment.options.configDiagnostics {
		_, err := fmt.Fprintln(stdout, environment.configSummary)
		return err
	}
	stores, err := openApplicationStores(ctx, configDirectory, environment)
	if err != nil {
		return err
	}
	defer stores.close()
	if environment.options.listSessions {
		return listSessions(stores.sessions, environment.options.workspace, stdout)
	}
	runtime, err := assembleAgentRuntime(ctx, environment, stores, policyStore, eventLogger, lookupEnv, stdin)
	if err != nil {
		return err
	}
	defer runtime.close()
	return executeInvocation(ctx, executionContext{
		environment: environment, stores: stores, runtime: runtime,
		policyStore: policyStore, eventLogger: eventLogger,
		promptArgs: startup.promptArgs, initialImages: startup.initialImages,
		stdin: stdin, stdout: stdout, stderr: stderr,
	})
}
