package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
)

func (m model) submit(prompt string) (tea.Model, tea.Cmd) {
	if prompt == "" && len(m.draftPastes) == 0 || m.busy || m.runner == nil {
		return m, nil
	}
	effectivePrompt, displayPrompt := m.promptWithPastes(prompt)
	if len(m.draftContexts) > 0 {
		effectivePrompt = strings.TrimSpace(effectivePrompt + "\n\nAttached context:\n\n" + strings.Join(m.draftContexts, "\n\n"))
	}
	if len(m.draftImageLabels) > 0 || len(m.draftContexts) > 0 {
		displayPrompt += fmt.Sprintf("\n[%d image(s), %d context file(s) attached]", len(m.draftImageLabels), len(m.draftContexts))
	}
	images := append([]provider.Image(nil), m.draftImages...)
	m.draftImages, m.draftImageLabels, m.draftContexts, m.draftPastes = nil, nil, nil, nil
	m.messages = append(m.messages, message{role: "user", content: displayPrompt}, message{role: "assistant", streaming: true})
	m.appendSessionEvent("user_prompt", map[string]string{"content": displayPrompt})
	m.busy = true
	m.turnStarted = time.Now()
	m.input.Placeholder = "Send a message after the next tool call…"
	m.refreshMessages(true)
	requestContext, cancel := context.WithCancel(m.ctx)
	m.cancelCurrentRequest = cancel
	skillInstructions := ""
	if m.skills != nil {
		skillInstructions = m.skills.Instructions(prompt)
	}
	memoryInstructions := ""
	if m.memory != nil {
		if captured, err := m.memory.AutoCapture(prompt); err != nil {
			m.addError("Auto-capture memory: " + err.Error())
		} else if captured {
			m.addStatus("Captured a preference in long-term memory.")
		}
		memoryInstructions = m.memory.Instructions()
	}
	extraInstructions := prompts.Turn(prompts.TurnInput{
		Model: m.options.Model, Mode: m.collaborationMode, Approval: string(m.options.Approval), SandboxStatus: m.options.SandboxStatus,
		Workspace: m.options.Workspace, ContextWindowTokens: m.options.ContextWindowTokens, AutoCompactTokens: m.options.AutoCompactTokens,
		UsableContextTokens: m.options.UsableContextTokens, MaxTurns: m.options.MaxTurns, Skills: skillInstructions,
		Memory: memoryInstructions, Plugins: m.options.Plugins, Hooks: m.options.HookSummary, MCPServers: m.mcpServerNames(), Goal: m.goalPrompt(),
	})
	run := m.runner.Start(requestContext, agent.Input{Prompt: effectivePrompt, History: append([]provider.Message(nil), m.history...), Instructions: extraInstructions, Images: images})
	m.activeRun, m.agentEvents = run, run.Events
	return m, tea.Batch(m.spinner.Tick, nextAgentEvent(run.Events), m.input.Focus())
}

func (m model) promptWithPastes(prompt string) (effective, display string) {
	effective, display = strings.TrimSpace(prompt), strings.TrimSpace(prompt)
	if len(m.draftPastes) == 0 {
		return effective, display
	}
	sections := make([]string, 0, len(m.draftPastes)+1)
	if effective != "" {
		sections = append(sections, effective)
	}
	for _, pasted := range m.draftPastes {
		if strings.TrimSpace(pasted) != "" {
			sections = append(sections, pasted)
		}
	}
	// Folding is only a composer presentation detail. Submitted paste behaves
	// like text entered directly so transcripts and resumed sessions show it.
	effective = strings.Join(sections, "\n\n")
	return effective, effective
}

func (m model) handleAgentEvent(event agent.Event) (tea.Model, tea.Cmd) {
	switch event.Type {
	case agent.EventTextDelta:
		m.appendAssistantDelta(event.Delta)
	case agent.EventToolStarted:
		m.finishStreamingAssistant()
		callID := ""
		if event.Call != nil {
			callID = event.Call.ID
		}
		content := formatToolEvent(event, false)
		m.messages = append(m.messages, message{role: "tool", callID: callID, content: content, baseContent: content, toolStarted: time.Now(), toolRunning: true})
		if event.Call != nil {
			m.appendSessionEvent("tool_started", map[string]any{"id": event.Call.ID, "name": event.Call.Name, "arguments": event.Call.Arguments, "risk": event.Risk})
		}
	case agent.EventToolOutputDelta:
		if event.Call != nil && event.Delta != "" {
			for index := len(m.messages) - 1; index >= 0; index-- {
				item := &m.messages[index]
				if item.role != "tool" || item.callID != event.Call.ID {
					continue
				}
				item.toolOutput = boundedLiveToolOutput(item.toolOutput + event.Delta)
				item.content = item.baseContent + renderLiveToolOutput(item.toolOutput)
				item.copyContent = item.toolOutput
				item.rendered = ""
				break
			}
		}
	case agent.EventApprovalRequired:
		m.pendingApproval = event.Approval
		m.approvalChoice = 0
		m.input.Blur()
		m.resize(m.width, m.height)
		return m, nil
	case agent.EventToolFinished:
		if event.Result != nil && event.Result.IsError {
			m.toolFailed++
		} else {
			m.toolSucceeded++
		}
		content := formatToolEvent(event, true)
		updated := false
		if event.Call != nil {
			for index := len(m.messages) - 1; index >= 0; index-- {
				if m.messages[index].role == "tool" && m.messages[index].callID == event.Call.ID {
					m.messages[index].content = content
					m.messages[index].baseContent = content
					m.messages[index].toolRunning = false
					m.messages[index].rendered = ""
					if event.Result != nil {
						m.messages[index].copyContent = event.Result.Content
					}
					updated = true
					break
				}
			}
		}
		if !updated {
			copyContent := ""
			if event.Result != nil {
				copyContent = event.Result.Content
			}
			m.messages = append(m.messages, message{role: "tool", content: content, copyContent: copyContent})
		}
		if event.Call != nil && event.Result != nil {
			m.appendSessionEvent("tool_finished", map[string]any{"id": event.Call.ID, "name": event.Call.Name, "error": event.Result.IsError, "content": event.Result.Content})
		}
		m.resize(m.width, m.height)
	case agent.EventQueuedCommitted:
		for index, queued := range event.Queued {
			visible := queued
			if index < len(m.queuedMessages) {
				visible = m.queuedMessages[index]
			}
			m.messages = append(m.messages, message{role: "user", content: visible})
		}
		if len(event.Queued) >= len(m.queuedMessages) {
			m.queuedMessages = nil
		} else {
			m.queuedMessages = m.queuedMessages[len(event.Queued):]
		}
		m.input.Placeholder = "Send a message after the next tool call…"
		m.resize(m.width, m.height)
	case agent.EventContextCompacted:
		m.appendSessionEvent("context_compacted", map[string]int{"before_tokens": event.BeforeTokens, "after_tokens": event.AfterTokens})
		m.addStatus(fmt.Sprintf("Context compacted automatically: %d → %d estimated tokens.", event.BeforeTokens, event.AfterTokens))
	case agent.EventCompleted:
		m.finishStreamingAssistant()
		if event.Response != nil {
			m.inputTokens += event.Response.Usage.InputTokens
			m.outputTokens += event.Response.Usage.OutputTokens
		}
		if !m.turnStarted.IsZero() {
			m.lastLatency = time.Since(m.turnStarted)
		}
		m.history = append([]provider.Message(nil), event.History...)
		m.saveSession()
		m.refreshMessages(true)
		finished, command := m.finish(nil)
		m = finished.(model)
		if prompt, ok := m.nextGoalContinuation(); ok {
			m.goalContinuations++
			m.addStatus(fmt.Sprintf("Continuing active goal automatically (%d/20).", m.goalContinuations))
			return m.submit(prompt)
		}
		return m, tea.Batch(command, terminalNotification(m.options.Notification, "SuperCode finished"))
	case agent.EventError:
		m.appendSessionEvent("turn_error", map[string]string{"error": event.Err.Error()})
		m.messages = append(m.messages, message{role: "error", content: event.Err.Error()})
		m.refreshMessages(true)
		finished, command := m.finish(event.Err)
		return finished, tea.Batch(command, terminalNotification(m.options.Notification, "SuperCode needs attention"))
	}
	m.refreshMessages(true)
	return m, nextAgentEvent(m.agentEvents)
}

func (m *model) refreshRunningToolCells() {
	changed := false
	for index := range m.messages {
		item := &m.messages[index]
		if !item.toolRunning || item.toolStarted.IsZero() {
			continue
		}
		elapsed := time.Since(item.toolStarted).Round(time.Second)
		item.content = item.baseContent + renderLiveToolOutput(item.toolOutput) + "\n" + statusStyle.Render("    Running for "+elapsed.String()+"…")
		item.rendered = ""
		changed = true
	}
	if changed {
		m.refreshMessages(true)
	}
}

func (m model) continueAfterApproval() (tea.Model, tea.Cmd) {
	m.pendingApproval = nil
	m.approvalChoice = 0
	m.input.Placeholder = "Send a message after the next tool call…"
	m.resize(m.width, m.height)
	return m, tea.Batch(nextAgentEvent(m.agentEvents), m.input.Focus())
}

func (m model) finish(_ error) (tea.Model, tea.Cmd) {
	if m.cancelCurrentRequest != nil {
		m.cancelCurrentRequest()
		m.cancelCurrentRequest = nil
	}
	m.busy, m.pendingApproval, m.activeRun = false, nil, nil
	m.queuedMessages = nil
	m.input.Placeholder = "Ask anything or type /help…"
	m.refreshMessages(true)
	return m, m.input.Focus()
}

func (m model) stopCurrentTurn() (tea.Model, tea.Cmd) {
	if m.cancelCurrentRequest != nil {
		m.cancelCurrentRequest()
		m.cancelCurrentRequest = nil
	}
	m.finishStreamingAssistant()
	for index := range m.messages {
		item := &m.messages[index]
		if !item.toolRunning {
			continue
		}
		item.toolRunning = false
		item.content = item.baseContent + renderLiveToolOutput(item.toolOutput) + "\n" + statusStyle.Render("    Stopped by user.")
		item.rendered = ""
	}
	m.appendSessionEvent("turn_interrupted", nil)
	m.busy, m.pendingApproval, m.activeRun = false, nil, nil
	m.agentEvents = nil
	m.queuedMessages = nil
	m.input.Placeholder = "Ask anything or type /help…"
	m.addStatus("Generation stopped. You can send another message.")
	m.resize(m.width, m.height)
	return m, m.input.Focus()
}

func (m *model) nextGoalContinuation() (string, bool) {
	if !m.options.GoalAutoContinue || m.taskState == nil || m.goalContinuations >= 20 {
		return "", false
	}
	_, goal := m.taskState.Snapshot()
	if goal == nil || goal.Status != "active" {
		m.goalContinuations = 0
		return "", false
	}
	if goal.TokenBudget > 0 && goal.TotalTokens >= int64(goal.TokenBudget) {
		m.addStatus("Goal auto-continuation stopped because its token budget was exhausted.")
		return "", false
	}
	return prompts.GoalContinuation(goal.Objective, goal.TotalTokens, int64(goal.TokenBudget)), true
}

func (m *model) setCollaborationMode(mode prompts.Mode) {
	mode = prompts.NormalizeMode(string(mode))
	m.collaborationMode = mode
	m.planMode = mode == prompts.ModePlan
	m.session.Mode = string(mode)
}

func (m model) mcpServerNames() []string {
	if m.options.Tools == nil {
		return nil
	}
	servers := make(map[string]struct{})
	for _, name := range m.options.Tools.Names() {
		parts := strings.Split(name, "__")
		if len(parts) >= 3 && parts[0] == "mcp" && strings.TrimSpace(parts[1]) != "" {
			servers[parts[1]] = struct{}{}
		}
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m model) goalPrompt() string {
	if m.taskState == nil {
		return ""
	}
	_, goal := m.taskState.Snapshot()
	if goal == nil {
		return ""
	}
	budget := "unlimited"
	if goal.TokenBudget > 0 {
		budget = fmt.Sprintf("%d tokens (%d remaining)", goal.TokenBudget, max(int64(0), int64(goal.TokenBudget)-goal.TotalTokens))
	}
	return fmt.Sprintf("Objective: %s\nStatus: %s\nUsage: %d tokens across %d turns\nBudget: %s", goal.Objective, goal.Status, goal.TotalTokens, goal.Turns, budget)
}
