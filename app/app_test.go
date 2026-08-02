package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/modelcatalog"
	"github.com/daemon365/supercode/internal/provider"
	openaiProvider "github.com/daemon365/supercode/internal/provider/openai"
)

type fakeProvider struct {
	streams  []provider.Stream
	requests []provider.Request
}

func (fakeProvider) Generate(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, errors.New("unexpected Generate call")
}

func (fake *fakeProvider) Stream(_ context.Context, request provider.Request) (provider.Stream, error) {
	fake.requests = append(fake.requests, request)
	if len(fake.streams) == 0 {
		return nil, errors.New("no fake stream available")
	}
	stream := fake.streams[0]
	fake.streams = fake.streams[1:]
	return stream, nil
}

type fakeStream struct {
	events  []provider.StreamEvent
	current int
}

func (stream *fakeStream) Next() bool {
	if stream.current >= len(stream.events) {
		return false
	}
	stream.current++
	return true
}

func (stream *fakeStream) Current() provider.StreamEvent {
	return stream.events[stream.current-1]
}

func (*fakeStream) Err() error   { return nil }
func (*fakeStream) Close() error { return nil }

func TestReadPrompt(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{name: "arguments", args: []string{"explain", "this"}, stdin: "ignored", want: "explain this"},
		{name: "stdin", stdin: "  explain stdin\n", want: "explain stdin"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := readPrompt(test.args, strings.NewReader(test.stdin))
			if err != nil {
				t.Fatalf("readPrompt() error: %v", err)
			}
			if got != test.want {
				t.Fatalf("readPrompt() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReadPromptRejectsEmptyInput(t *testing.T) {
	_, err := readPrompt(nil, strings.NewReader(" \n"))
	if err == nil {
		t.Fatal("readPrompt() error = nil, want an error")
	}
}

func TestRunRequiresAPIKey(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	lookupEnv := func(name string) (string, bool) {
		if name == "SUPERCODE_CONFIG" {
			return configPath, true
		}
		return "", false
	}
	err := Run(
		context.Background(),
		[]string{"hello"},
		strings.NewReader(""),
		io.Discard,
		io.Discard,
		lookupEnv,
	)
	if !errors.Is(err, openaiProvider.ErrMissingAPIKey) {
		t.Fatalf("run() error = %v, want %v", err, openaiProvider.ErrMissingAPIKey)
	}
}

func TestRunStreamsFromOpenAICompatibleChatEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Errorf("request path = %q, want /chat/completions", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"compatible-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello \"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(writer, "data: {\"id\":\"chatcmpl_test\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"compatible-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configContents := "url: " + server.URL + "\nmodel: compatible-model\ntoken: test-key\nstream: true\ntimeout: 2s\n"
	if err := os.WriteFile(configPath, []byte(configContents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	lookupEnv := func(name string) (string, bool) {
		if name == "SUPERCODE_CONFIG" {
			return configPath, true
		}
		return "", false
	}

	var stdout strings.Builder
	err = Run(
		context.Background(),
		[]string{"hello"},
		strings.NewReader(""),
		&stdout,
		io.Discard,
		lookupEnv,
	)
	if err != nil {
		t.Fatalf("run() error: %v", err)
	}
	if stdout.String() != "hello world\n" {
		t.Fatalf("stdout = %q, want hello world", stdout.String())
	}
}

func TestParseOptionsPrecedence(t *testing.T) {
	stream := false
	fileConfig := config.File{
		URL:     "https://config.example/v1",
		Model:   "config-model",
		Stream:  &stream,
		Timeout: "45s",
	}
	lookupEnv := func(name string) (string, bool) {
		switch name {
		case "OPENAI_BASE_URL":
			return "https://environment.example/v1", true
		case "OPENAI_MODEL":
			return "environment-model", true
		case "SUPERCODE_TIMEOUT":
			return "30s", true
		default:
			return "", false
		}
	}

	got, _, err := parseOptionsWithConfig([]string{
		"-model", "flag-model",
		"-stream=true",
		"hello",
	}, io.Discard, lookupEnv, fileConfig)
	if err != nil {
		t.Fatalf("parseOptionsWithConfig() error: %v", err)
	}
	if got.modelName != "flag-model" {
		t.Fatalf("model = %q, want flag-model", got.modelName)
	}
	if got.baseURL != "https://environment.example/v1" {
		t.Fatalf("base URL = %q, want environment URL", got.baseURL)
	}
	if !got.stream {
		t.Fatal("stream = false, want flag value true")
	}
	if got.timeout != 30*time.Second {
		t.Fatalf("timeout = %v, want 30s", got.timeout)
	}
}

func TestEnvOrDefault(t *testing.T) {
	lookupEnv := func(name string) (string, bool) {
		if name == "SET" {
			return "custom", true
		}
		return "", false
	}

	if got := envOrDefault(lookupEnv, "SET", "fallback"); got != "custom" {
		t.Fatalf("envOrDefault() = %q, want custom", got)
	}
	if got := envOrDefault(lookupEnv, "MISSING", "fallback"); got != "fallback" {
		t.Fatalf("envOrDefault() = %q, want fallback", got)
	}
}

func TestResolveAPIKeyPrecedenceAndTokenCommand(t *testing.T) {
	fromEnvironment, err := resolveAPIKey(context.Background(), config.File{
		Token: "plain-key", TokenCommand: []string{"command-that-must-not-run"},
	}, func(name string) (string, bool) {
		if name == "OPENAI_API_KEY" {
			return "environment-key", true
		}
		return "", false
	})
	if err != nil || fromEnvironment != "environment-key" {
		t.Fatalf("environment key = %q, err = %v", fromEnvironment, err)
	}

	fromCommand, err := resolveAPIKey(context.Background(), config.File{
		Token: "plain-key", TokenCommand: []string{"/bin/sh", "-c", "printf command-key"},
	}, func(string) (string, bool) { return "", false })
	if err != nil || fromCommand != "command-key" {
		t.Fatalf("command key = %q, err = %v", fromCommand, err)
	}
}

func TestResolveAPIKeyBoundsCommandOutput(t *testing.T) {
	_, err := resolveAPIKey(context.Background(), config.File{
		TokenCommand: []string{"/bin/sh", "-c", "printf '%020000d' 0; exit 1"},
	}, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("resolveAPIKey() error = nil")
	}
	if len(err.Error()) > 5*1024 {
		t.Fatalf("error was not bounded: %d bytes", len(err.Error()))
	}
}

func TestParseOptionsBaseURLPrecedence(t *testing.T) {
	lookupEnv := func(name string) (string, bool) {
		if name == "OPENAI_BASE_URL" {
			return "https://environment.example/v1", true
		}
		return "", false
	}

	fromEnvironment, promptArgs, err := parseOptions([]string{"hello"}, io.Discard, lookupEnv)
	if err != nil {
		t.Fatalf("parseOptions() environment error: %v", err)
	}
	if fromEnvironment.baseURL != "https://environment.example/v1" {
		t.Fatalf("base URL = %q, want environment URL", fromEnvironment.baseURL)
	}
	if len(promptArgs) != 1 || promptArgs[0] != "hello" {
		t.Fatalf("prompt args = %v, want [hello]", promptArgs)
	}

	fromFlag, _, err := parseOptions([]string{
		"-base-url", "http://127.0.0.1:8000/v1",
		"hello",
	}, io.Discard, lookupEnv)
	if err != nil {
		t.Fatalf("parseOptions() flag error: %v", err)
	}
	if fromFlag.baseURL != "http://127.0.0.1:8000/v1" {
		t.Fatalf("base URL = %q, want flag URL", fromFlag.baseURL)
	}
}

func TestParseOptionsStream(t *testing.T) {
	lookupEnv := func(string) (string, bool) { return "", false }

	defaults, _, err := parseOptions([]string{"hello"}, io.Discard, lookupEnv)
	if err != nil {
		t.Fatalf("parseOptions() default error: %v", err)
	}
	if !defaults.stream {
		t.Fatal("stream default = false, want true")
	}

	disabled, _, err := parseOptions([]string{"-stream=false", "hello"}, io.Discard, lookupEnv)
	if err != nil {
		t.Fatalf("parseOptions() disabled error: %v", err)
	}
	if disabled.stream {
		t.Fatal("stream = true, want false")
	}
}

func TestParseOptionsAllowsUnlimitedTurns(t *testing.T) {
	options, _, err := parseOptions([]string{"-max-turns=0", "hello"}, io.Discard, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if options.maxTurns != 0 {
		t.Fatalf("max turns = %d, want unlimited (0)", options.maxTurns)
	}
	if options.contextWindowTokens != 272_000 || options.autoCompactTokens != 244_800 || options.usableContextTokens != 258_400 {
		t.Fatalf("context defaults = %d/%d/%d", options.contextWindowTokens, options.autoCompactTokens, options.usableContextTokens)
	}
	if !options.alternateScreen {
		t.Fatal("alternate screen should be enabled by default")
	}
	inline, _, err := parseOptions([]string{"--no-alt-screen", "hello"}, io.Discard, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if inline.alternateScreen {
		t.Fatal("--no-alt-screen was ignored")
	}
}

func TestParseOptionsUsesSelectedModelCatalogLimits(t *testing.T) {
	configuration := config.File{
		Model: "small",
		ModelCatalog: map[string]modelcatalog.Capabilities{
			"small": {ContextWindowTokens: 32000, AutoCompactTokens: 28000, UsableContextTokens: 30000},
			"large": {ContextWindowTokens: 200000, AutoCompactTokens: 170000, UsableContextTokens: 190000},
		},
	}
	parsed, _, err := parseOptionsWithConfig([]string{"--model", "large", "hello"}, io.Discard, func(string) (string, bool) { return "", false }, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.contextWindowTokens != 200000 || parsed.autoCompactTokens != 170000 || parsed.usableContextTokens != 190000 {
		t.Fatalf("catalog limits = %d/%d/%d", parsed.contextWindowTokens, parsed.autoCompactTokens, parsed.usableContextTokens)
	}
}

func TestCobraSubcommands(t *testing.T) {
	lookupEnv := func(string) (string, bool) { return "", false }

	chatOptions, prompt, err := parseOptions([]string{"chat", "hello"}, io.Discard, lookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !chatOptions.chat || len(prompt) != 1 || prompt[0] != "hello" {
		t.Fatalf("chat options = %#v, prompt = %v", chatOptions, prompt)
	}

	sessionOptions, _, err := parseOptions([]string{"sessions"}, io.Discard, lookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !sessionOptions.listSessions {
		t.Fatal("sessions subcommand did not enable session listing")
	}

	initOptions, _, err := parseOptions([]string{"config", "init"}, io.Discard, lookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !initOptions.initConfig {
		t.Fatal("config init did not enable config initialization")
	}

	diagnosticOptions, _, err := parseOptions([]string{"config", "diagnostics"}, io.Discard, lookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnosticOptions.configDiagnostics {
		t.Fatal("config diagnostics did not enable diagnostics")
	}
}

func TestCobraHelp(t *testing.T) {
	var output bytes.Buffer
	got, prompt, err := parseOptions([]string{"--help"}, &output, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if !got.helpShown || len(prompt) != 0 {
		t.Fatalf("options = %#v, prompt = %v", got, prompt)
	}
	for _, expected := range []string{"Usage:", "Available Commands:", "chat", "sessions", "config", "completion"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("help output does not contain %q:\n%s", expected, output.String())
		}
	}
}

func TestWriteStream(t *testing.T) {
	finalResponse := provider.Response{ID: "resp_test", Text: "hello world"}
	modelProvider := &fakeProvider{streams: []provider.Stream{&fakeStream{events: []provider.StreamEvent{
		{Type: provider.StreamEventTextDelta, Delta: "hello "},
		{Type: provider.StreamEventTextDelta, Delta: "world"},
		{Type: provider.StreamEventCompleted, Response: &finalResponse},
	}}}}

	var output strings.Builder
	response, err := writeStream(context.Background(), modelProvider, provider.Request{
		Model:  "gpt-test",
		Prompt: "hello",
	}, &output)
	if err != nil {
		t.Fatalf("writeStream() error: %v", err)
	}
	if output.String() != "hello world\n" {
		t.Fatalf("stream output = %q, want %q", output.String(), "hello world\\n")
	}
	if response.ID != "resp_test" {
		t.Fatalf("response ID = %q, want resp_test", response.ID)
	}
}

func TestRunChatContinuesConversationWithHistory(t *testing.T) {
	first := provider.Response{ID: "resp_first", Text: "first answer"}
	second := provider.Response{ID: "resp_second", Text: "second answer"}
	modelProvider := &fakeProvider{streams: []provider.Stream{
		&fakeStream{events: []provider.StreamEvent{
			{Type: provider.StreamEventTextDelta, Delta: "first answer"},
			{Type: provider.StreamEventCompleted, Response: &first},
		}},
		&fakeStream{events: []provider.StreamEvent{
			{Type: provider.StreamEventTextDelta, Delta: "second answer"},
			{Type: provider.StreamEventCompleted, Response: &second},
		}},
	}}

	var stdout strings.Builder
	var stderr strings.Builder
	err := runChat(
		context.Background(),
		modelProvider,
		options{modelName: "gpt-test", stream: true, timeout: time.Second},
		nil,
		strings.NewReader("first question\nsecond question\n/exit\n"),
		&stdout,
		&stderr,
		false,
	)
	if err != nil {
		t.Fatalf("runChat() error: %v", err)
	}
	if stdout.String() != "first answer\nsecond answer\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if len(modelProvider.requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(modelProvider.requests))
	}
	if len(modelProvider.requests[0].History) != 0 {
		t.Fatalf("first history = %+v, want empty", modelProvider.requests[0].History)
	}
	if len(modelProvider.requests[1].History) != 2 {
		t.Fatalf("second history = %+v, want two messages", modelProvider.requests[1].History)
	}
	if modelProvider.requests[1].History[0].Role != provider.MessageRoleUser || modelProvider.requests[1].History[0].Content != "first question" {
		t.Fatalf("first history message = %+v", modelProvider.requests[1].History[0])
	}
	if modelProvider.requests[1].History[1].Role != provider.MessageRoleAssistant || modelProvider.requests[1].History[1].Content != "first answer" {
		t.Fatalf("second history message = %+v", modelProvider.requests[1].History[1])
	}
}

func TestRunChatNewResetsConversation(t *testing.T) {
	first := provider.Response{ID: "resp_first", Text: "first answer"}
	second := provider.Response{ID: "resp_second", Text: "second answer"}
	modelProvider := &fakeProvider{streams: []provider.Stream{
		&fakeStream{events: []provider.StreamEvent{
			{Type: provider.StreamEventCompleted, Response: &first},
		}},
		&fakeStream{events: []provider.StreamEvent{
			{Type: provider.StreamEventCompleted, Response: &second},
		}},
	}}

	err := runChat(
		context.Background(),
		modelProvider,
		options{modelName: "gpt-test", stream: true, timeout: time.Second},
		nil,
		strings.NewReader("first question\n/new\nsecond question\n/exit\n"),
		io.Discard,
		io.Discard,
		false,
	)
	if err != nil {
		t.Fatalf("runChat() error: %v", err)
	}
	if len(modelProvider.requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(modelProvider.requests))
	}
	if len(modelProvider.requests[1].History) != 0 {
		t.Fatalf("second history = %+v, want empty", modelProvider.requests[1].History)
	}
}
