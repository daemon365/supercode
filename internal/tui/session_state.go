package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daemon365/supercode/internal/collaboration"
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
)

const (
	sessionActionEvent      = "event"
	sessionActionSave       = "save"
	sessionActionComplete   = "complete"
	sessionActionError      = "error"
	sessionActionInterrupt  = "interrupt"
	sessionActionFork       = "fork"
	sessionActionBacktrack  = "backtrack"
	sessionActionArchive    = "archive"
	sessionActionRename     = "rename"
	sessionActionDelete     = "delete"
	sessionActionList       = "list"
	sessionActionLoad       = "load"
	sessionActionLoadLatest = "load_latest"
	sessionActionActivate   = "activate"
	sessionActionNew        = "new"
	sessionActionExit       = "exit"
)

type sessionJob struct {
	action   string
	blocking bool
	run      func() sessionJobResult
}

type sessionJobResult struct {
	action         string
	value          session.Session
	payload        any
	warnings       []string
	agentsRestored bool
	err            error
}

type sessionJobMsg struct{ result sessionJobResult }

func restoreSessionAgents(manager *collaboration.Manager, data json.RawMessage) error {
	if manager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return manager.RestoreContext(ctx, data)
}

func (m *model) prepareSession() (session.Session, error) {
	if m.store == nil {
		return m.session, nil
	}
	if m.session.ID == "" {
		created, err := m.store.New(m.options.Workspace, m.options.Model)
		if err != nil {
			return session.Session{}, err
		}
		m.session = created
	}
	m.session.Messages = append([]provider.Message(nil), m.history...)
	m.session.Mode = string(prompts.NormalizeMode(string(m.collaborationMode)))
	if m.taskState != nil {
		m.session.Plan, m.session.Goal = m.taskState.Snapshot()
	}
	if m.collaboration != nil {
		m.session.Agents = m.collaboration.Snapshot()
	}
	if m.session.Title == "" {
		for _, item := range m.history {
			if item.Role == provider.MessageRoleUser {
				m.session.Title = truncateTitle(item.Content)
				break
			}
		}
	}
	return m.session, nil
}

func (m *model) enqueueSessionJob(job sessionJob) tea.Cmd {
	if job.run == nil {
		return nil
	}
	if job.blocking {
		m.sessionBlocking++
	}
	m.sessionJobs = append(m.sessionJobs, job)
	return m.nextSessionJob()
}

func (m *model) nextSessionJob() tea.Cmd {
	if m.sessionJobRunning || len(m.sessionJobs) == 0 {
		return nil
	}
	m.sessionJobRunning = true
	run := m.sessionJobs[0].run
	return func() tea.Msg { return sessionJobMsg{result: run()} }
}

func (m *model) enqueueSessionEvent(eventType string, payload any) tea.Cmd {
	if m.store == nil || m.session.ID == "" {
		return nil
	}
	store, id := m.store, m.session.ID
	return m.enqueueSessionJob(sessionJob{action: sessionActionEvent, run: func() sessionJobResult {
		return sessionJobResult{action: sessionActionEvent, err: store.Append(id, eventType, payload)}
	}})
}

func (m *model) enqueueSessionSave(action string, blocking bool, eventType string, payload any) (tea.Cmd, error) {
	value, err := m.prepareSession()
	if err != nil {
		return nil, err
	}
	if m.store == nil {
		return m.enqueueSessionJob(sessionJob{action: action, blocking: blocking, run: func() sessionJobResult {
			return sessionJobResult{action: action, value: value}
		}}), nil
	}
	store := m.store
	return m.enqueueSessionJob(sessionJob{action: action, blocking: blocking, run: func() sessionJobResult {
		var failures []error
		if eventType != "" {
			if appendErr := store.Append(value.ID, eventType, payload); appendErr != nil {
				failures = append(failures, appendErr)
			}
		}
		if commitErr := store.Commit(value); commitErr != nil {
			failures = append(failures, commitErr)
		}
		return sessionJobResult{action: action, value: value, err: errors.Join(failures...)}
	}}), nil
}

func (m model) handleSessionJob(message sessionJobMsg) (tea.Model, tea.Cmd) {
	if len(m.sessionJobs) > 0 {
		job := m.sessionJobs[0]
		m.sessionJobs = m.sessionJobs[1:]
		if job.blocking && m.sessionBlocking > 0 {
			m.sessionBlocking--
		}
	}
	m.sessionJobRunning = false
	result := message.result
	var command tea.Cmd
	switch result.action {
	case sessionActionEvent:
		if result.err != nil {
			m.addError("Append session event: " + result.err.Error())
		}
	case sessionActionSave:
		if result.err != nil {
			m.addError("Save session: " + result.err.Error())
		}
	case sessionActionExit:
		m.exitSessionSaved = true
		m.exitSessionFailed = result.err != nil
		if result.err != nil {
			m.addError("Save session before exit: " + result.err.Error())
		}
	case sessionActionComplete:
		m.pendingCompletion = false
		if result.err != nil {
			m.addError("Save completed turn: " + result.err.Error())
		}
		finished, focus := m.finish(result.err)
		m = finished.(model)
		if m.exiting {
			command = nil
		} else if result.err == nil {
			if prompt, ok := m.nextGoalContinuation(); ok {
				m.goalContinuations++
				m.addStatus(fmt.Sprintf("Continuing active goal automatically (%d/20).", m.goalContinuations))
				return m.submit(prompt)
			}
			command = tea.Batch(focus, terminalNotification(m.options.Notification, "SuperCode finished"))
		} else {
			command = tea.Batch(focus, terminalNotification(m.options.Notification, "SuperCode needs attention"))
		}
	case sessionActionError:
		m.pendingCompletion = false
		if result.err != nil {
			m.addError("Save failed turn: " + result.err.Error())
		}
		finished, focus := m.finish(result.err)
		m = finished.(model)
		if !m.exiting {
			command = tea.Batch(focus, terminalNotification(m.options.Notification, "SuperCode needs attention"))
		}
	case sessionActionInterrupt:
		m.cancelSavePending = false
		if result.err != nil {
			m.addError("Save interrupted turn: " + result.err.Error())
		}
		if m.cancelRunDone {
			var focus tea.Cmd
			m, focus = m.completeCancellation()
			command = focus
		}
	default:
		m, command = m.handleSessionActionResult(result)
	}
	next := m.nextSessionJob()
	return m, tea.Batch(command, next)
}

func (m *model) loadHistory(history []provider.Message) {
	m.history = append([]provider.Message(nil), history...)
	m.messages = nil
	m.resetTranscriptCache()
	for _, item := range history {
		if (item.Role == provider.MessageRoleUser || item.Role == provider.MessageRoleAssistant) && item.Content != "" {
			m.messages = append(m.messages, message{role: string(item.Role), content: item.Content})
		}
		if item.Role == provider.MessageRoleUser {
			m.rememberInput(item.Content)
		}
	}
	m.refreshMessages(true)
}
