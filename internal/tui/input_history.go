package tui

import "strings"

const (
	maximumInputHistoryEntries    = 100
	maximumInputHistoryEntryBytes = 64 * 1024
)

func (m *model) rememberInput(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	value = boundedTextSuffix(value, maximumInputHistoryEntryBytes)
	if len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != value {
		m.inputHistory = append(m.inputHistory, value)
		if len(m.inputHistory) > maximumInputHistoryEntries {
			m.inputHistory = append([]string(nil), m.inputHistory[len(m.inputHistory)-maximumInputHistoryEntries:]...)
		}
	}
	m.inputHistoryCursor, m.inputHistoryDraft = -1, ""
}

// navigateInputHistory gives logical multiline cursor motion priority. At the
// first/last line, Up/Down traverses submitted inputs and restores the draft
// that was present before navigation began.
func (m *model) navigateInputHistory(direction int) bool {
	if direction < 0 {
		if m.input.LineCount() > 1 && m.input.Line() > 0 {
			return false
		}
		if len(m.inputHistory) == 0 {
			return false
		}
		if m.inputHistoryCursor < 0 {
			m.inputHistoryDraft = m.input.Value()
			m.inputHistoryCursor = len(m.inputHistory) - 1
		} else if m.inputHistoryCursor > 0 {
			m.inputHistoryCursor--
		}
		m.input.SetValue(m.inputHistory[m.inputHistoryCursor])
		m.input.MoveToEnd()
		return true
	}
	if direction > 0 {
		if m.inputHistoryCursor < 0 {
			return false
		}
		if m.input.LineCount() > 1 && m.input.Line() < m.input.LineCount()-1 {
			return false
		}
		if m.inputHistoryCursor < len(m.inputHistory)-1 {
			m.inputHistoryCursor++
			m.input.SetValue(m.inputHistory[m.inputHistoryCursor])
		} else {
			m.inputHistoryCursor = -1
			m.input.SetValue(m.inputHistoryDraft)
			m.inputHistoryDraft = ""
		}
		m.input.MoveToEnd()
		return true
	}
	return false
}
