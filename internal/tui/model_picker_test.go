package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daemon365/supercode/internal/provider"
)

func TestPickerWindowFollowsSelection(t *testing.T) {
	tests := []struct {
		name               string
		choice             int
		count              int
		limit              int
		wantStart, wantEnd int
	}{
		{name: "first row", choice: 0, count: 12, limit: 8, wantStart: 0, wantEnd: 8},
		{name: "last row on first view", choice: 7, count: 12, limit: 8, wantStart: 0, wantEnd: 8},
		{name: "first row past first view", choice: 8, count: 12, limit: 8, wantStart: 1, wantEnd: 9},
		{name: "last row", choice: 11, count: 12, limit: 8, wantStart: 4, wantEnd: 12},
		{name: "empty", choice: 0, count: 0, limit: 8, wantStart: 0, wantEnd: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start, end := pickerWindow(test.choice, test.count, test.limit)
			if start != test.wantStart || end != test.wantEnd {
				t.Fatalf("pickerWindow(%d, %d, %d) = (%d, %d), want (%d, %d)", test.choice, test.count, test.limit, start, end, test.wantStart, test.wantEnd)
			}
		})
	}
}

func TestRenderModelPickerShowsProviderSuffix(t *testing.T) {
	m := model{
		showModelPicker: true,
		modelChoices:    []string{"copilot/gpt-4"},
		options: Options{Model: "copilot/gpt-4", ModelInfos: []provider.ModelInfo{{
			Selector: "copilot/gpt-4", ID: "gpt-4", Provider: "copilot",
		}}},
	}
	view := ansi.Strip(m.renderModelPicker(80))
	if !strings.Contains(view, "gpt-4 (current) [in copilot]") {
		t.Fatalf("view =\n%s", view)
	}
}

func TestRenderModelPickerShowsChoicePastFirstView(t *testing.T) {
	choices := make([]string, 12)
	for index := range choices {
		choices[index] = fmt.Sprintf("choice-%02d", index)
	}
	m := model{
		showModelPicker: true,
		modelChoices:    choices,
		modelChoice:     8,
	}

	view := ansi.Strip(m.renderModelPicker(80))
	if !strings.Contains(view, "› choice-08") {
		t.Fatalf("selected model is not visible:\n%s", view)
	}
	if strings.Contains(view, "choice-00") {
		t.Fatalf("first model remained visible after scrolling:\n%s", view)
	}
	if !strings.Contains(view, "2-9 of 12") {
		t.Fatalf("visible range is missing:\n%s", view)
	}
}
