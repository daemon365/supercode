package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// slashInvocation parses a command once before routing it to a focused
// handler. Handlers own one product area instead of growing one giant switch.
type slashInvocation struct {
	Raw    string
	Name   string
	Fields []string
}

type slashRoute uint8

const (
	routeUnknown slashRoute = iota
	routeSession
	routeRuntime
	routeOutput
	routeWorkflow
	routeMemory
)

func parseSlashInvocation(value string) slashInvocation {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return slashInvocation{}
	}
	return slashInvocation{Raw: value, Name: strings.ToLower(fields[0]), Fields: fields}
}

func (m model) command(value string) (tea.Model, tea.Cmd) {
	invocation := parseSlashInvocation(value)
	switch routeSlashCommand(invocation.Name) {
	case routeSession:
		return m.commandSession(invocation)
	case routeRuntime:
		return m.commandRuntime(invocation)
	case routeOutput:
		return m.commandOutput(invocation)
	case routeWorkflow:
		return m.commandWorkflow(invocation)
	case routeMemory:
		return m.commandMemoryAndResume(invocation)
	default:
		message := "Unknown command " + invocation.Name + ". Type / to browse commands."
		if suggestion := similarSlashCommand(invocation.Name); suggestion != "" {
			message += " Did you mean " + suggestion + "?"
		}
		m.addError(message)
		return m, nil
	}
}

func routeSlashCommand(name string) slashRoute {
	switch name {
	case "/exit", "/quit", "/help", "/editor", "/clear", "/new", "/rename", "/fork", "/backtrack", "/archive", "/delete":
		return routeSession
	case "/status", "/tools", "/mcp", "/plugins", "/hooks", "/agents", "/config", "/model", "/permissions", "/reasoning", "/service-tier", "/theme", "/keymap", "/compact", "/review", "/diff", "/mention", "/image", "/detach":
		return routeRuntime
	case "/copy", "/queue", "/raw", "/markdown":
		return routeOutput
	case "/ps", "/stop", "/plan", "/mode", "/goal", "/skills":
		return routeWorkflow
	case "/memory", "/remember", "/forget", "/sessions", "/resume":
		return routeMemory
	default:
		return routeUnknown
	}
}
