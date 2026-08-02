package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
)

func (m model) commandSession(invocation slashInvocation) (tea.Model, tea.Cmd) {
	value, fields := invocation.Raw, invocation.Fields
	switch invocation.Name {
	case "/exit", "/quit":
		return m.beginExit()
	case "/help":
		m.showHelp = true
		m.refreshMessages(false)
		m.viewport.GotoTop()
	case "/editor":
		command, err := editDraftCommand(strings.TrimSpace(strings.TrimPrefix(value, fields[0])))
		if err != nil {
			m.addError(err.Error())
			break
		}
		m.input.Blur()
		return m, command
	case "/clear":
		m.messages = nil
		m.resetTranscriptCache()
		m.refreshMessages(false)
	case "/new":
		current, err := m.prepareSession()
		if err != nil {
			m.addError("Prepare current session: " + err.Error())
			break
		}
		store, manager := m.store, m.collaboration
		workspace, modelName := m.options.Workspace, m.options.Model
		command := m.enqueueSessionJob(sessionJob{action: sessionActionNew, blocking: true, run: func() sessionJobResult {
			var created session.Session
			var err error
			if store != nil {
				if current.ID != "" {
					err = store.Commit(current)
				}
				if err == nil {
					created, err = store.New(workspace, modelName)
				}
				if err == nil {
					err = store.Commit(created)
				}
			}
			if err == nil {
				err = restoreSessionAgents(manager, json.RawMessage(`[]`))
			}
			return sessionJobResult{action: sessionActionNew, value: created, agentsRestored: err == nil, err: err}
		}})
		return m, command
	case "/rename":
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		title := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		if title == "" {
			m.addError("Usage: /rename <title>")
			break
		}
		store, id := m.store, m.session.ID
		command := m.enqueueSessionJob(sessionJob{action: sessionActionRename, blocking: true, run: func() sessionJobResult {
			updated, err := store.Rename(id, title)
			return sessionJobResult{action: sessionActionRename, value: updated, err: err}
		}})
		return m, command
	case "/fork":
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		current, err := m.prepareSession()
		if err != nil {
			m.addError("Prepare session fork: " + err.Error())
			break
		}
		store, manager := m.store, m.collaboration
		command := m.enqueueSessionJob(sessionJob{action: sessionActionFork, blocking: true, run: func() sessionJobResult {
			if err := store.Commit(current); err != nil {
				return sessionJobResult{action: sessionActionFork, err: fmt.Errorf("save before fork: %w", err)}
			}
			forked, err := store.Fork(current.ID)
			if err == nil {
				err = restoreSessionAgents(manager, forked.Agents)
			}
			return sessionJobResult{action: sessionActionFork, value: forked, agentsRestored: err == nil, err: err}
		}})
		return m, command
	case "/backtrack":
		var userIndices []int
		for index, item := range m.history {
			if item.Role == provider.MessageRoleUser {
				userIndices = append(userIndices, index)
			}
		}
		if len(fields) == 1 {
			lines := []string{"User turns (use /backtrack <turn> to fork before one):"}
			start := max(0, len(userIndices)-20)
			for turn := start; turn < len(userIndices); turn++ {
				lines = append(lines, fmt.Sprintf("%d. %s", turn+1, preview(strings.Join(strings.Fields(m.history[userIndices[turn]].Content), " "), 100)))
			}
			if len(userIndices) == 0 {
				lines = append(lines, "No user turns are available.")
			}
			m.addStatus(strings.Join(lines, "\n"))
			break
		}
		turn, err := strconv.Atoi(fields[1])
		if err != nil || turn < 1 || turn > len(userIndices) {
			m.addError(fmt.Sprintf("Turn must be between 1 and %d.", len(userIndices)))
			break
		}
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		current, err := m.prepareSession()
		if err != nil {
			m.addError("Prepare session backtrack: " + err.Error())
			break
		}
		selectedIndex := userIndices[turn-1]
		selectedPrompt := m.history[selectedIndex].Content
		truncated := append([]provider.Message(nil), m.history[:selectedIndex]...)
		store, manager := m.store, m.collaboration
		command := m.enqueueSessionJob(sessionJob{action: sessionActionBacktrack, blocking: true, run: func() sessionJobResult {
			if err := store.Commit(current); err != nil {
				return sessionJobResult{action: sessionActionBacktrack, err: fmt.Errorf("save before backtrack: %w", err)}
			}
			forked, err := store.Fork(current.ID)
			if err != nil {
				return sessionJobResult{action: sessionActionBacktrack, err: err}
			}
			forked.Messages = truncated
			if err := store.Commit(forked); err != nil {
				return sessionJobResult{action: sessionActionBacktrack, err: fmt.Errorf("save backtracked fork: %w", err)}
			}
			if err := restoreSessionAgents(manager, forked.Agents); err != nil {
				return sessionJobResult{action: sessionActionBacktrack, value: forked, err: err}
			}
			return sessionJobResult{action: sessionActionBacktrack, value: forked, payload: backtrackResult{prompt: selectedPrompt, turn: turn}, agentsRestored: true}
		}})
		return m, command
	case "/archive":
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		current, err := m.prepareSession()
		if err != nil {
			m.addError("Prepare session archive: " + err.Error())
			break
		}
		store, workspace, modelName, manager := m.store, m.options.Workspace, m.options.Model, m.collaboration
		command := m.enqueueSessionJob(sessionJob{action: sessionActionArchive, blocking: true, run: func() sessionJobResult {
			if err := store.Commit(current); err != nil {
				return sessionJobResult{action: sessionActionArchive, err: fmt.Errorf("save before archive: %w", err)}
			}
			if _, err := store.Archive(current.ID); err != nil {
				return sessionJobResult{action: sessionActionArchive, err: err}
			}
			created, err := store.New(workspace, modelName)
			if err == nil {
				err = store.Commit(created)
			}
			if err == nil {
				err = restoreSessionAgents(manager, created.Agents)
			}
			return sessionJobResult{action: sessionActionArchive, value: created, payload: current.ID, agentsRestored: err == nil, err: err}
		}})
		return m, command
	case "/delete":
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		if len(fields) != 2 || strings.ToLower(fields[1]) != "confirm" {
			m.addError("This permanently removes the session. Type /delete confirm to continue.")
			break
		}
		deletedID := m.session.ID
		store, workspace, modelName, manager := m.store, m.options.Workspace, m.options.Model, m.collaboration
		command := m.enqueueSessionJob(sessionJob{action: sessionActionDelete, blocking: true, run: func() sessionJobResult {
			if err := store.Delete(deletedID); err != nil {
				return sessionJobResult{action: sessionActionDelete, err: err}
			}
			created, err := store.New(workspace, modelName)
			if err == nil {
				err = store.Commit(created)
			}
			if err == nil {
				err = restoreSessionAgents(manager, created.Agents)
			}
			return sessionJobResult{action: sessionActionDelete, value: created, payload: deletedID, agentsRestored: err == nil, err: err}
		}})
		return m, command
	}
	return m, nil
}

type backtrackResult struct {
	prompt string
	turn   int
}

type sessionActivationContext struct {
	request    uint64
	fromPicker bool
	latest     bool
}

func (m model) handleSessionActionResult(result sessionJobResult) (model, tea.Cmd) {
	if result.action == sessionActionList {
		listed, ok := result.payload.(sessionListResult)
		if !ok || listed.request != m.sessionPickerRequest || !m.showSessionPicker {
			return m, nil
		}
		m.sessionPickerLoading = false
		m.sessionIncludeAll = listed.includeArchived
		m.sessionWarnings = append([]string(nil), result.warnings...)
		if len(result.warnings) > 0 {
			m.addStatus("Session index warning:\n- " + strings.Join(result.warnings, "\n- "))
		}
		if result.err != nil {
			m.sessionPickerError = result.err.Error()
			m.addError("List sessions: " + result.err.Error())
		} else {
			m.sessionChoices = append([]session.Metadata(nil), listed.items...)
			m.sessionPickerError = ""
		}
		m.resize(m.width, m.height)
		return m, nil
	}
	if result.action == sessionActionLoad {
		loadContext, fromPicker := result.payload.(sessionLoadContext)
		if fromPicker && (loadContext.request != m.sessionPickerRequest || !m.showSessionPicker) {
			return m, nil
		}
		if result.err != nil {
			if fromPicker {
				m.sessionPickerLoading = false
				m.sessionPickerError = result.err.Error()
				m.resize(m.width, m.height)
			}
			m.addError("Load session: " + result.err.Error())
			return m, nil
		}
		return m.beginSessionActivation(result.value, sessionActivationContext{
			request: loadContext.request, fromPicker: fromPicker,
		})
	}
	if result.action == sessionActionLoadLatest {
		if result.err != nil {
			m.addError("Load latest session: " + result.err.Error())
			return m, nil
		}
		return m.beginSessionActivation(result.value, sessionActivationContext{latest: true})
	}
	if result.action == sessionActionActivate {
		activation, _ := result.payload.(sessionActivationContext)
		if activation.fromPicker && (activation.request != m.sessionPickerRequest || !m.showSessionPicker) {
			return m, nil
		}
		m.sessionPickerActivating = false
		if result.err != nil {
			if activation.fromPicker {
				m.sessionPickerLoading = false
				m.sessionPickerError = result.err.Error()
				m.resize(m.width, m.height)
			}
			m.addError("Activate session: " + result.err.Error())
			return m, nil
		}
		if err := m.resumeSession(result.value, result.agentsRestored); err != nil {
			if activation.fromPicker {
				m.sessionPickerLoading = false
				m.sessionPickerError = err.Error()
				m.resize(m.width, m.height)
			}
			m.addError(err.Error())
			return m, nil
		}
		if activation.fromPicker {
			m.showSessionPicker = false
			m.sessionPickerLoading = false
			m.resize(m.width, m.height)
		}
		m.addStatus("Resumed session " + result.value.ID + ".")
		return m, m.input.Focus()
	}
	if result.err != nil {
		m.addError(result.err.Error())
		return m, nil
	}
	switch result.action {
	case sessionActionNew:
		m.session = result.value
		m.history, m.messages = nil, nil
		m.resetTranscriptCache()
		m.setCollaborationMode(prompts.ModeDefault)
		if m.taskState != nil {
			m.taskState.Reset()
		}
		m.resize(m.width, m.height)
		m.addStatus("Started a new session.")
	case sessionActionRename:
		m.session = result.value
		m.addStatus("Renamed session to " + result.value.Title + ".")
	case sessionActionFork:
		if err := m.resumeSession(result.value, result.agentsRestored); err != nil {
			m.addError(err.Error())
			return m, nil
		}
		m.addStatus("Forked session " + result.value.ID + ".")
	case sessionActionBacktrack:
		value, _ := result.payload.(backtrackResult)
		if err := m.resumeSession(result.value, result.agentsRestored); err != nil {
			m.addError(err.Error())
			return m, nil
		}
		m.input.SetValue(value.prompt)
		m.input.MoveToEnd()
		m.inputHistoryCursor, m.inputHistoryDraft = -1, ""
		m.addStatus(fmt.Sprintf("Forked from user turn %d. Edit the restored prompt and press Enter.", value.turn))
	case sessionActionArchive, sessionActionDelete:
		identifier, _ := result.payload.(string)
		m.session = result.value
		m.history, m.messages = nil, nil
		m.resetTranscriptCache()
		m.setCollaborationMode(prompts.ModeDefault)
		if m.taskState != nil {
			m.taskState.Reset()
		}
		verb := "Archived"
		if result.action == sessionActionDelete {
			verb = "Deleted"
		}
		m.addStatus(verb + " session " + identifier + " and started a new session.")
	}
	return m, m.input.Focus()
}

func (m model) beginSessionActivation(loaded session.Session, activation sessionActivationContext) (model, tea.Cmd) {
	if loaded.Workspace != m.options.Workspace {
		failure := "the session belongs to a different workspace"
		if activation.fromPicker {
			m.sessionPickerLoading = false
			m.sessionPickerError = failure
			m.resize(m.width, m.height)
		}
		m.addError(failure)
		return m, nil
	}
	if activation.fromPicker {
		m.sessionPickerActivating = true
	}
	current, err := m.prepareSession()
	if err != nil {
		m.sessionPickerActivating = false
		if activation.fromPicker {
			m.sessionPickerLoading = false
			m.sessionPickerError = err.Error()
			m.resize(m.width, m.height)
		}
		m.addError("Prepare current session: " + err.Error())
		return m, nil
	}
	manager, store := m.collaboration, m.store
	command := m.enqueueSessionJob(sessionJob{action: sessionActionActivate, blocking: true, run: func() sessionJobResult {
		active := loaded
		var err error
		if manager != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err = manager.Quiesce(ctx)
			cancel()
			if err != nil {
				err = fmt.Errorf("stop current sub-agents before switching: %w", err)
			} else {
				current.Agents = manager.Snapshot()
			}
		}
		if store != nil && current.ID != "" {
			if err == nil {
				err = store.Commit(current)
			}
			if err != nil {
				err = fmt.Errorf("save current session before switching: %w", err)
			} else if current.ID == loaded.ID {
				active, err = store.Load(loaded.ID)
				if err != nil {
					err = fmt.Errorf("reload current session after saving: %w", err)
				}
			}
		}
		if err == nil {
			err = restoreSessionAgents(manager, active.Agents)
		}
		return sessionJobResult{
			action: sessionActionActivate, value: active, payload: activation,
			agentsRestored: err == nil, err: err,
		}
	}})
	return m, command
}
