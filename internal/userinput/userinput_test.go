package userinput

import (
	"context"
	"strings"
	"testing"
)

func TestRequestToolWaitsForStructuredAnswer(t *testing.T) {
	manager := New()
	result := make(chan string, 1)
	go func() {
		value, err := manager.Tool().Execute(context.Background(), `{"questions":[{"header":"Style","id":"style","question":"Choose a style","options":[{"label":"Fast","description":"Less detail"},{"label":"Thorough","description":"More detail"}]}]}`)
		if err != nil {
			result <- err.Error()
			return
		}
		result <- value.Content
	}()
	request := <-manager.Requests()
	request.Decide(map[string]string{"style": "Thorough"})
	if got := <-result; !strings.Contains(got, `"style":"Thorough"`) {
		t.Fatalf("result = %s", got)
	}
}
