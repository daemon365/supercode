package tui

import (
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
)

func (m *model) saveSession() {
	if m.store == nil {
		return
	}
	if m.session.ID == "" {
		m.session, _ = m.store.New(m.options.Workspace, m.options.Model)
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
	checkpoint := session.Checkpoint{Messages: m.session.Messages, Plan: m.session.Plan, Goal: m.session.Goal, Agents: m.session.Agents, Title: m.session.Title, Mode: m.session.Mode}
	if err := m.store.Append(m.session.ID, "checkpoint", checkpoint); err != nil {
		m.messages = append(m.messages, message{role: "error", content: "Append session checkpoint: " + err.Error()})
		return
	}
	if err := m.store.Save(m.session); err != nil {
		m.messages = append(m.messages, message{role: "error", content: "Save session: " + err.Error()})
	}
}

func (m *model) appendSessionEvent(eventType string, payload any) {
	if m.store == nil || m.session.ID == "" {
		return
	}
	if err := m.store.Append(m.session.ID, eventType, payload); err != nil {
		m.messages = append(m.messages, message{role: "error", content: "Append session event: " + err.Error()})
	}
}

func (m *model) loadHistory(history []provider.Message) {
	m.history = append([]provider.Message(nil), history...)
	m.messages = nil
	for _, item := range history {
		if (item.Role == provider.MessageRoleUser || item.Role == provider.MessageRoleAssistant) && item.Content != "" {
			m.messages = append(m.messages, message{role: string(item.Role), content: item.Content})
		}
	}
	m.refreshMessages(true)
}
