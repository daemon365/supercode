package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	exitVisibleDelay  = 40 * time.Millisecond
	exitMessageDelay  = 100 * time.Millisecond
	exitTimeout       = 2 * time.Minute
	memoryExitTimeout = 90 * time.Second
)

func (m model) beginExit() (tea.Model, tea.Cmd) {
	if m.exiting {
		if m.exitCancel != nil {
			m.exitCancel()
		}
		if m.cancelCurrentRequest != nil {
			m.cancelCurrentRequest()
		}
		return m, tea.Quit
	}
	m.exiting = true
	m.input.Blur()
	m.addStatus("Saving session and memory before exit… Press Ctrl+C again to force quit.")

	var stopCommand tea.Cmd
	if m.busy && !m.pendingCompletion {
		if !m.cancelling {
			updated, command := m.stopCurrentTurn()
			m, stopCommand = updated.(model), command
		}
	}
	visibleCommand := func() tea.Msg {
		timer := time.NewTimer(exitVisibleDelay)
		defer timer.Stop()
		<-timer.C
		return exitVisibleMsg{}
	}
	timeoutCommand := func() tea.Msg {
		timer := time.NewTimer(exitTimeout)
		defer timer.Stop()
		<-timer.C
		return exitTimeoutMsg{}
	}
	return m, tea.Batch(stopCommand, visibleCommand, timeoutCommand)
}

func (m model) advanceExit(command tea.Cmd) (tea.Model, tea.Cmd) {
	if !m.exiting || !m.exitVisible {
		return m, command
	}
	if m.busy || m.cancelling || m.turnPreparing || m.pendingCompletion {
		return m, command
	}
	if m.sessionJobRunning || len(m.sessionJobs) > 0 || m.sessionBlocking > 0 {
		return m, command
	}
	if !m.exitAgentsStarted {
		m.exitAgentsStarted = true
		if m.collaboration == nil {
			return m.advanceExit(command)
		}
		m.exitAgentsPending = true
		manager := m.collaboration
		agentCommand := func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return exitAgentsMsg{err: manager.Shutdown(ctx)}
		}
		return m, tea.Batch(command, agentCommand)
	}
	if m.exitAgentsPending {
		return m, command
	}
	if !m.exitSessionStarted {
		m.exitSessionStarted = true
		saveCommand, err := m.enqueueSessionSave(sessionActionExit, true, "session_exit", nil)
		if err != nil {
			m.exitSessionSaved, m.exitSessionFailed = true, true
			m.addError("Prepare session exit save: " + err.Error())
			return m.advanceExit(command)
		}
		return m, tea.Batch(command, saveCommand)
	}
	if !m.exitSessionSaved || m.attachmentJobs > 0 || m.runtimeJobs > 0 {
		return m, command
	}
	if !m.exitMemoryStarted {
		m.exitMemoryStarted = true
		if m.memory == nil || m.modelProvider == nil {
			return m.scheduleExit(command)
		}
		shutdownContext, cancel := context.WithTimeout(context.Background(), memoryExitTimeout)
		m.exitCancel = cancel
		m.exitMemoryPending = true
		memoryStore, providerClient, sessions := m.memory, m.modelProvider, m.store
		sessionID, modelName, saveFailed := m.session.ID, m.options.Model, m.exitSessionFailed
		memoryCommand := func() tea.Msg {
			defer cancel()
			var err error
			if saveFailed || sessions == nil || sessionID == "" {
				err = memoryStore.StopStartup(shutdownContext)
			} else {
				_, err = memoryStore.RunShutdown(shutdownContext, providerClient, sessions, sessionID, modelName)
			}
			return exitMemoryMsg{err: err}
		}
		return m, tea.Batch(command, memoryCommand)
	}
	if m.exitMemoryPending {
		return m, command
	}
	return m.scheduleExit(command)
}

func (m model) scheduleExit(command tea.Cmd) (tea.Model, tea.Cmd) {
	if m.exitQuitScheduled {
		return m, command
	}
	m.exitQuitScheduled = true
	quitCommand := func() tea.Msg {
		timer := time.NewTimer(exitMessageDelay)
		defer timer.Stop()
		<-timer.C
		return exitQuitMsg{}
	}
	return m, tea.Batch(command, quitCommand)
}
