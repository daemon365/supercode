package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/diagnostic"
	"github.com/daemon365/supercode/internal/policy"
)

func newEventLogger(options options) (func(agent.Event), error) {
	if options.debugLog == "" {
		return func(agent.Event) {}, nil
	}
	logger, err := diagnostic.NewLogger(options.debugLog)
	if err != nil {
		return nil, fmt.Errorf("open debug log: %w", err)
	}
	logger.Log("startup", map[string]any{"workspace": options.workspace, "model": options.modelName, "version": "0.1.0"})
	return func(event agent.Event) {
		fields := map[string]any{"type": event.Type, "risk": event.Risk, "summary": event.Summary, "delta_bytes": len(event.Delta)}
		if event.Call != nil {
			fields["tool"] = event.Call.Name
		}
		if event.Result != nil {
			fields["result_bytes"], fields["result_error"] = len(event.Result.Content), event.Result.IsError
		}
		if event.Err != nil {
			fields["error"] = event.Err.Error()
		}
		logger.Log("agent_event", fields)
	}, nil
}

func runManagementCommand(
	ctx context.Context,
	startup startupState,
	configDirectory string,
	policyStore *policy.Store,
	stdout io.Writer,
	lookupEnv func(string) (string, bool),
) (bool, error) {
	options := startup.options
	switch {
	case options.policyAction != "":
		return true, runPolicyCommand(policyStore, options.policyAction, options.policyValues, stdout)
	case options.doctor:
		return true, runDoctor(startup.configPath, options.workspace, startup.fileConfig, policyStore, stdout, lookupEnv)
	case options.diagnosticExport != "":
		return true, exportDiagnostics(options.diagnosticExport, startup.configPath, options.workspace, startup.fileConfig, policyStore, lookupEnv, stdout)
	case options.mcpAction != "":
		return true, runMCPCommand(ctx, startup.configPath, options.workspace, startup.userConfig, options, stdout)
	case options.skillAction != "":
		return true, runSkillCommand(filepath.Join(configDirectory, "skills"), options.skillAction, options.skillValues, stdout)
	case options.pluginAction != "":
		return true, runPluginCommand(filepath.Join(configDirectory, "plugins"), options.pluginAction, options.pluginValues, stdout)
	case options.hookAction != "":
		return true, runHookCommand(startup.configPath, options.workspace, startup.userConfig, options.hookAction, options.hookValues, stdout)
	default:
		return false, nil
	}
}
