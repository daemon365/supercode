package memory

import (
	"strings"
	"time"
)

const (
	citationOpen  = "<oai-mem-citation>"
	citationClose = "</oai-mem-citation>"
)

// CitationParser removes hidden memory citations across arbitrary stream chunk
// boundaries while retaining their rollout IDs for usage feedback.
type CitationParser struct {
	pending   string
	hidden    bool
	body      strings.Builder
	citations []string
}

func (p *CitationParser) Push(value string) string {
	p.pending += value
	var visible strings.Builder
	for p.pending != "" {
		if p.hidden {
			if index := strings.Index(p.pending, citationClose); index >= 0 {
				p.body.WriteString(p.pending[:index])
				p.citations = append(p.citations, p.body.String())
				p.body.Reset()
				p.pending = p.pending[index+len(citationClose):]
				p.hidden = false
				continue
			}
			keep := suffixPrefixLength(p.pending, citationClose)
			p.body.WriteString(p.pending[:len(p.pending)-keep])
			p.pending = p.pending[len(p.pending)-keep:]
			break
		}
		if index := strings.Index(p.pending, citationOpen); index >= 0 {
			visible.WriteString(p.pending[:index])
			p.pending = p.pending[index+len(citationOpen):]
			p.hidden = true
			continue
		}
		keep := suffixPrefixLength(p.pending, citationOpen)
		visible.WriteString(p.pending[:len(p.pending)-keep])
		p.pending = p.pending[len(p.pending)-keep:]
		break
	}
	return visible.String()
}

func (p *CitationParser) Finish() (string, []string) {
	visible := ""
	if p.hidden {
		p.body.WriteString(p.pending)
		p.citations = append(p.citations, p.body.String())
	} else {
		visible = p.pending
	}
	p.pending = ""
	p.hidden = false
	p.body.Reset()
	ids := rolloutIDs(p.citations)
	p.citations = nil
	return visible, ids
}

func StripCitations(value string) (string, []string) {
	var parser CitationParser
	visible := parser.Push(value)
	tail, ids := parser.Finish()
	return visible + tail, ids
}

func suffixPrefixLength(value, marker string) int {
	maximum := min(len(value), len(marker)-1)
	for length := maximum; length > 0; length-- {
		if strings.HasSuffix(value, marker[:length]) {
			return length
		}
	}
	return 0
}

func rolloutIDs(citations []string) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, citation := range citations {
		start := strings.Index(citation, "<rollout_ids>")
		end := strings.Index(citation, "</rollout_ids>")
		if start < 0 || end < start {
			start = strings.Index(citation, "<thread_ids>")
			end = strings.Index(citation, "</thread_ids>")
			if start < 0 || end < start {
				continue
			}
			start += len("<thread_ids>")
		} else {
			start += len("<rollout_ids>")
		}
		for _, line := range strings.Split(citation[start:end], "\n") {
			id := strings.TrimSpace(line)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Store) RecordUsage(rolloutIDs []string) {
	if s == nil || len(rolloutIDs) == 0 {
		return
	}
	wanted := make(map[string]struct{}, len(rolloutIDs))
	for _, id := range rolloutIDs {
		wanted[strings.TrimSpace(id)] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadStateUnlocked()
	if err != nil {
		return
	}
	now := time.Now().UTC()
	changed := false
	for sessionID, record := range state.Rollouts {
		if _, ok := wanted[record.RolloutID]; !ok {
			continue
		}
		record.UsageCount++
		record.LastUsage = &now
		state.Rollouts[sessionID] = record
		changed = true
	}
	if changed {
		_ = s.saveStateUnlocked(state)
	}
}
