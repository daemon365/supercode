package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/attachment"
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/tool"
)

func (m model) commandRuntime(invocation slashInvocation) (tea.Model, tea.Cmd) {
	value, fields := invocation.Raw, invocation.Fields
	switch invocation.Name {
	case "/status":
		toolCount, skillCount := 0, 0
		if m.options.Tools != nil {
			toolCount = m.options.Tools.Len()
		}
		if m.skills != nil {
			skillCount = m.skills.Len()
		}
		sandboxStatus := m.options.SandboxStatus
		if sandboxStatus == "" {
			sandboxStatus = tool.SandboxStatus(m.options.Workspace)
		}
		m.addStatus(fmt.Sprintf("Model: %s\nReasoning effort: %s\nService tier: %s\nContext: %d nominal / %d compact / %d usable tokens\nWorkspace: %s\nSandbox: %s\nApproval: %s\nSession: %s\nTools: %d\nSkills: %d\nUsage: %d input / %d output tokens\nTool results: %d succeeded / %d failed\nLast turn latency: %s", m.options.Model, defaultString(m.options.ReasoningEffort, "provider default"), defaultString(m.options.ServiceTier, "provider default"), m.options.ContextWindowTokens, m.options.AutoCompactTokens, m.options.UsableContextTokens, m.options.Workspace, sandboxStatus, m.options.Approval, m.session.ID, toolCount, skillCount, m.inputTokens, m.outputTokens, m.toolSucceeded, m.toolFailed, m.lastLatency.Round(time.Millisecond)))
	case "/tools":
		names := []string{}
		if m.options.Tools != nil {
			names = m.options.Tools.Names()
		}
		if len(names) == 0 {
			m.addStatus("No tools are available.")
		} else {
			m.addStatus("Available tools (" + strconv.Itoa(len(names)) + "):\n  " + strings.Join(names, "\n  "))
		}
	case "/mcp":
		servers := make(map[string]int)
		if m.options.Tools != nil {
			for _, name := range m.options.Tools.Names() {
				parts := strings.Split(name, "__")
				if len(parts) >= 3 && parts[0] == "mcp" {
					servers[parts[1]]++
				}
			}
		}
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, fmt.Sprintf("%s — %d entries", name, servers[name]))
		}
		sort.Strings(names)
		if len(names) == 0 {
			names = append(names, "No connected MCP servers.")
		}
		m.addStatus("MCP servers:\n  " + strings.Join(names, "\n  "))
	case "/plugins":
		values := m.options.Plugins
		if len(values) == 0 {
			values = []string{"No enabled plugins."}
		}
		m.addStatus("Plugins:\n  " + strings.Join(values, "\n  "))
	case "/hooks":
		values := m.options.HookSummary
		if len(values) == 0 {
			values = []string{"No enabled hooks."}
		}
		m.addStatus("Hooks:\n  " + strings.Join(values, "\n  "))
	case "/agents":
		if m.collaboration == nil {
			m.addStatus("No sub-agents.")
			break
		}
		var values []struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			Role      string `json:"role"`
			Model     string `json:"model"`
			Reasoning string `json:"reasoning_effort"`
			Output    string `json:"output"`
			Error     string `json:"error"`
		}
		_ = json.Unmarshal(m.collaboration.Snapshot(), &values)
		if len(fields) > 1 {
			target := strings.TrimSpace(fields[1])
			for _, item := range values {
				if item.Name != target {
					continue
				}
				body := fmt.Sprintf("### Agent `%s`\n\n- Status: %s\n- Role: %s\n- Model: %s\n- Reasoning: %s", item.Name, item.Status, defaultString(item.Role, "worker"), defaultString(item.Model, "inherited"), defaultString(item.Reasoning, "inherited"))
				if strings.TrimSpace(item.Output) != "" {
					body += "\n\n" + item.Output
				}
				if item.Error != "" {
					body += "\n\n**Error:** " + item.Error
				}
				m.messages = append(m.messages, message{role: "assistant", content: body})
				m.refreshMessages(true)
				return m, nil
			}
			m.addError("Agent " + target + " was not found.")
			break
		}
		lines := []string{"Agent tree:"}
		for _, item := range values {
			indent := strings.Repeat("  ", strings.Count(item.Name, "/")+1)
			lines = append(lines, fmt.Sprintf("%s└─ %s  [%s]  %s", indent, item.Name, item.Status, defaultString(item.Role, "worker")))
		}
		if len(values) == 0 {
			lines = append(lines, "  No sub-agents.")
		}
		m.addStatus(strings.Join(lines, "\n"))
	case "/config":
		if strings.TrimSpace(m.options.ConfigSummary) == "" {
			m.addStatus("Configuration source diagnostics are unavailable.")
		} else {
			m.addStatus(m.options.ConfigSummary)
		}
	case "/model":
		if len(fields) == 1 {
			m.openModelPicker()
			break
		}
		modelName := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		if err := m.runner.SetModel(modelName); err != nil {
			m.addError(err.Error())
			break
		}
		m.options.Model = modelName
		m.syncModelLimits()
		m.session.Model = modelName
		m.saveSession()
		m.addStatus("Model changed to " + modelName + ".")
	case "/permissions":
		if len(fields) == 1 {
			m.addStatus("Current approval policy: " + string(m.options.Approval))
			break
		}
		mode, err := agent.ParseApprovalMode(fields[1])
		if err != nil {
			m.addError(err.Error())
			break
		}
		if err := m.runner.SetApproval(mode); err != nil {
			m.addError(err.Error())
			break
		}
		m.options.Approval = mode
		m.addStatus("Approval policy changed to " + string(mode) + ".")
	case "/reasoning":
		if len(fields) == 1 {
			m.addStatus("Reasoning effort: " + defaultString(m.options.ReasoningEffort, "provider default"))
			break
		}
		value := strings.ToLower(fields[1])
		if value == "default" {
			value = ""
		}
		if value != "" && value != "low" && value != "medium" && value != "high" && value != "xhigh" {
			m.addError("Usage: /reasoning [default|low|medium|high|xhigh]")
			break
		}
		m.options.ReasoningEffort = value
		m.runner.SetReasoningEffort(value)
		m.addStatus("Reasoning effort changed to " + defaultString(value, "provider default") + ".")
	case "/service-tier":
		if len(fields) == 1 {
			m.addStatus("Service tier: " + defaultString(m.options.ServiceTier, "provider default"))
			break
		}
		value := strings.ToLower(fields[1])
		if value == "provider" {
			value = ""
		}
		if value != "" && value != "auto" && value != "default" && value != "flex" && value != "scale" && value != "priority" && value != "fast" {
			m.addError("Usage: /service-tier [provider|auto|default|flex|scale|priority|fast]")
			break
		}
		m.options.ServiceTier = value
		m.runner.SetServiceTier(value)
		m.addStatus("Service tier changed to " + defaultString(value, "provider default") + ".")
	case "/theme":
		if len(fields) == 1 {
			m.addStatus("Theme: " + defaultString(m.options.Theme, "violet"))
			break
		}
		value := strings.ToLower(fields[1])
		if value != "violet" && value != "blue" && value != "green" && value != "mono" && value != "monochrome" {
			m.addError("Usage: /theme [violet|blue|green|mono]")
			break
		}
		m.options.Theme = value
		applyTheme(value)
		m.renderCacheWidth = 0
		m.resize(m.width, m.height)
	case "/keymap":
		if len(fields) == 1 {
			m.addStatus("Keymap: " + defaultString(m.options.Keymap, "standard"))
			break
		}
		value := strings.ToLower(fields[1])
		if value != "standard" && value != "vim" {
			m.addError("Usage: /keymap [standard|vim]")
			break
		}
		m.options.Keymap = value
		m.vimNormal = value == "vim"
		m.addStatus("Keymap changed to " + value + ".")
	case "/compact":
		before := agent.EstimateMessagesTokens(m.history)
		target := m.options.AutoCompactTokens / 2
		compacted, changed := agent.CompactHistory(m.history, target)
		if !changed {
			m.addStatus(fmt.Sprintf("Context is already compact (%d estimated tokens).", before))
			break
		}
		m.history = compacted
		m.saveSession()
		m.addStatus(fmt.Sprintf("Context compacted: %d → %d estimated tokens.", before, agent.EstimateMessagesTokens(compacted)))
	case "/review":
		focus := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		prompt := "Review the current Git changes for correctness, regressions, security issues, and missing tests. Inspect the diff including untracked files, run focused verification when useful, and report findings before any summary."
		if focus != "" {
			prompt += " Review focus: " + focus
		}
		return m.submit(prompt)
	case "/diff":
		arguments := `{}`
		if len(fields) > 1 && strings.EqualFold(fields[1], "staged") {
			arguments = `{"staged":true}`
		}
		m.executeToolForStatus("git_diff", arguments)
	case "/mention":
		path := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		if path == "" {
			m.addError("Usage: /mention <workspace-relative-path>")
			break
		}
		if m.options.Tools == nil {
			m.addError("File tools are unavailable.")
			break
		}
		reader, ok := m.options.Tools.Lookup("read_file")
		if !ok {
			m.addError("read_file is unavailable.")
			break
		}
		encodedPath, _ := json.Marshal(path)
		result, err := reader.Execute(m.ctx, `{"path":`+string(encodedPath)+`}`)
		if err != nil {
			m.addError(err.Error())
			break
		}
		m.draftContexts = append(m.draftContexts, "File: "+path+"\n```\n"+result.Content+"\n```")
		m.addStatus("Attached " + path + " to the next message.")
		m.resize(m.width, m.height)
	case "/image":
		path := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		if path == "" {
			m.addError("Usage: /image <workspace-path|clipboard>")
			break
		}
		if strings.EqualFold(path, "clipboard") {
			image, err := attachment.LoadClipboard("high")
			if err != nil {
				m.addError(err.Error())
				break
			}
			m.draftImages = append(m.draftImages, image)
			m.draftImageLabels = append(m.draftImageLabels, "clipboard")
		} else {
			if m.options.Tools == nil {
				m.addError("Image tools are unavailable.")
				break
			}
			viewer, ok := m.options.Tools.Lookup("view_image")
			if !ok {
				m.addError("view_image is unavailable.")
				break
			}
			encoded, _ := json.Marshal(path)
			result, err := viewer.Execute(m.ctx, `{"path":`+string(encoded)+`}`)
			if err != nil {
				m.addError(err.Error())
				break
			}
			m.draftImages = append(m.draftImages, result.Images...)
			m.draftImageLabels = append(m.draftImageLabels, path)
		}
		m.addStatus("Attached image to the next message.")
		m.resize(m.width, m.height)
	case "/detach":
		if len(fields) == 1 || strings.EqualFold(fields[1], "all") {
			m.draftImages, m.draftImageLabels, m.draftContexts, m.draftPastes = nil, nil, nil, nil
			m.addStatus("Removed all draft attachments.")
			m.resize(m.width, m.height)
			break
		}
		if strings.HasPrefix(strings.ToLower(fields[1]), "paste-") {
			index, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(fields[1]), "paste-"))
			if err != nil || index < 1 || index > len(m.draftPastes) {
				m.addError("Usage: /detach [all|image-number|paste-number]")
				break
			}
			m.draftPastes = append(m.draftPastes[:index-1], m.draftPastes[index:]...)
			m.addStatus(fmt.Sprintf("Removed pasted context %d.", index))
			m.resize(m.width, m.height)
			break
		}
		index, err := strconv.Atoi(fields[1])
		if err != nil || index < 1 || index > len(m.draftImages) {
			m.addError("Usage: /detach [all|image-number|paste-number]")
			break
		}
		m.draftImages = append(m.draftImages[:index-1], m.draftImages[index:]...)
		m.draftImageLabels = append(m.draftImageLabels[:index-1], m.draftImageLabels[index:]...)
		m.resize(m.width, m.height)
	}
	return m, nil
}

func (m model) commandWorkflow(invocation slashInvocation) (tea.Model, tea.Cmd) {
	fields := invocation.Fields
	switch invocation.Name {
	case "/ps":
		m.executeToolForStatus("list_processes", `{}`)
	case "/stop":
		if len(fields) != 2 {
			m.addError("Usage: /stop <session-id|all>")
			break
		}
		arguments := `{"all":true}`
		if !strings.EqualFold(fields[1], "all") {
			id, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				m.addError("Process session ID must be an integer or all.")
				break
			}
			arguments = fmt.Sprintf(`{"session_id":%d}`, id)
		}
		m.executeToolForStatus("stop_process", arguments)
	case "/plan":
		if len(fields) > 1 {
			switch strings.ToLower(fields[1]) {
			case "show":
				m.showPlan = true
			case "hide":
				m.showPlan = false
			case "on", "mode":
				m.setCollaborationMode(prompts.ModePlan)
				m.showPlan = true
				m.addStatus("Plan collaboration mode enabled.")
			case "off", "default":
				m.setCollaborationMode(prompts.ModeDefault)
				m.addStatus("Default collaboration mode enabled.")
			default:
				m.addError("Usage: /plan [on|off|show|hide]")
				return m, nil
			}
		} else {
			if m.collaborationMode == prompts.ModePlan {
				m.setCollaborationMode(prompts.ModeDefault)
			} else {
				m.setCollaborationMode(prompts.ModePlan)
			}
			m.showPlan = true
			m.addStatus("Collaboration mode: " + map[bool]string{true: "Plan", false: "Default"}[m.planMode] + ".")
		}
		m.saveSession()
		m.resize(m.width, m.height)
	case "/mode":
		if len(fields) == 1 {
			m.addStatus("Collaboration mode: " + string(m.collaborationMode) + ". Available modes: default, plan, execute, pair.")
			break
		}
		mode := prompts.NormalizeMode(fields[1])
		valid := strings.EqualFold(fields[1], "default") || strings.EqualFold(fields[1], "plan") || strings.EqualFold(fields[1], "execute") || strings.EqualFold(fields[1], "pair") || strings.EqualFold(fields[1], "pair-programming") || strings.EqualFold(fields[1], "pair_programming")
		if !valid {
			m.addError("Usage: /mode [default|plan|execute|pair]")
			break
		}
		m.setCollaborationMode(mode)
		if mode == prompts.ModePlan {
			m.showPlan = true
		}
		m.saveSession()
		m.resize(m.width, m.height)
		m.addStatus("Collaboration mode: " + string(mode) + ".")
	case "/goal":
		if m.taskState == nil {
			m.addStatus("No goal exists.")
			break
		}
		_, goal := m.taskState.Snapshot()
		if goal == nil {
			m.addStatus("No goal exists.")
		} else {
			budget := "unlimited"
			if goal.TokenBudget > 0 {
				budget = fmt.Sprintf("%d / %d tokens", goal.TotalTokens, goal.TokenBudget)
			} else if goal.TotalTokens > 0 {
				budget = fmt.Sprintf("%d tokens used", goal.TotalTokens)
			}
			m.addStatus(fmt.Sprintf("Goal\n  Objective: %s\n  Status: %s\n  Usage: %s\n  Turns: %d\n  Created: %s", goal.Objective, goal.Status, budget, goal.Turns, goal.CreatedAt.Local().Format(time.RFC3339)))
		}
	case "/skills":
		if len(fields) > 1 && strings.EqualFold(fields[1], "reload") {
			if m.skills == nil {
				m.addError("Skill catalog is unavailable.")
			} else if err := m.skills.Reload(); err != nil {
				m.addError(err.Error())
			} else {
				m.addStatus(fmt.Sprintf("Reloaded %d skill(s).", m.skills.Len()))
			}
			break
		}
		m.openSkillPicker()
	}
	return m, nil
}
