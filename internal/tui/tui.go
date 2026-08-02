package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/collaboration"
	"github.com/daemon365/supercode/internal/memory"
	"github.com/daemon365/supercode/internal/modelcatalog"
	"github.com/daemon365/supercode/internal/permission"
	"github.com/daemon365/supercode/internal/policy"
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
	"github.com/daemon365/supercode/internal/skill"
	"github.com/daemon365/supercode/internal/taskstate"
	"github.com/daemon365/supercode/internal/tool"
	"github.com/daemon365/supercode/internal/userinput"
)

type Options struct {
	Model               string
	Instructions        string
	InitialPrompt       string
	InitialImages       []provider.Image
	InitialImageLabels  []string
	Stream              bool
	Timeout             time.Duration
	MaxTurns            int
	ContextWindowTokens int
	AutoCompactTokens   int
	UsableContextTokens int
	ToolOutputTokens    int
	Approval            agent.ApprovalMode
	Permissions         *permission.Manager
	ApprovalCategories  map[tool.Category]bool
	ModelCatalog        *modelcatalog.Catalog
	Workspace           string
	Tools               *tool.Registry
	SessionStore        *session.Store
	Session             session.Session
	Skills              *skill.Catalog
	Memory              *memory.Store
	TaskState           *taskstate.State
	Hook                agent.Hook
	Collaboration       *collaboration.Manager
	GoalAutoContinue    bool
	AlternateScreen     bool
	Models              []string
	ReasoningEffort     string
	ServiceTier         string
	FallbackModels      []string
	ConfigSummary       string
	SandboxStatus       string
	Policy              *policy.Store
	UserInput           *userinput.Manager
	Plugins             []string
	HookSummary         []string
	Theme               string
	Keymap              string
	Notification        string
	TerminalTitle       string
	OnEvent             func(agent.Event)
}

type message struct {
	role, content string
	callID        string
	copyContent   string
	rendered      string
	streaming     bool
	baseContent   string
	toolStarted   time.Time
	toolRunning   bool
	toolOutput    string
}

type model struct {
	ctx                  context.Context
	runner               *agent.Runner
	options              Options
	input                textarea.Model
	viewport             viewport.Model
	spinner              spinner.Model
	initialFocus         tea.Cmd
	messages             []message
	width, height        int
	busy                 bool
	history              []provider.Message
	cancelCurrentRequest context.CancelFunc
	initialPrompt        string
	initialPromptFired   bool
	agentEvents          <-chan agent.Event
	activeRun            *agent.RunHandle
	queuedMessages       []string
	pendingApproval      *agent.ApprovalRequest
	approvalChoice       int
	store                *session.Store
	session              session.Session
	skills               *skill.Catalog
	memory               *memory.Store
	taskState            *taskstate.State
	showPlan             bool
	planMode             bool
	collaborationMode    prompts.Mode
	rawMode              bool
	showRawTranscript    bool
	showHelp             bool
	commandMenuDismissed bool
	commandChoice        int
	showSessionPicker    bool
	showModelPicker      bool
	modelQuery           string
	modelChoices         []string
	modelChoice          int
	showSkillPicker      bool
	skillQuery           string
	skillChoices         []skill.Skill
	skillChoice          int
	sessionQuery         string
	sessionChoices       []session.Session
	sessionChoice        int
	sessionIncludeAll    bool
	pendingUserInput     *userinput.Request
	userInputQuestion    int
	userInputChoice      int
	userInputAnswers     map[string]string
	userInputCustom      bool
	userInputDraft       string
	draftImages          []provider.Image
	draftImageLabels     []string
	draftContexts        []string
	draftPastes          []string
	collaboration        *collaboration.Manager
	goalContinuations    int
	turnStarted          time.Time
	inputTokens          int64
	outputTokens         int64
	toolSucceeded        int
	toolFailed           int
	lastLatency          time.Duration
	renderCacheWidth     int
	vimNormal            bool
}

type initialPromptMsg string
type agentEventMsg struct {
	events <-chan agent.Event
	event  agent.Event
	ok     bool
}
type userInputMsg struct {
	requests <-chan *userinput.Request
	request  *userinput.Request
}

var (
	accent       = lipgloss.Color("#8B5CF6")
	accentBright = lipgloss.Color("#C4B5FD")
	muted        = lipgloss.Color("#94A3B8")
	panel        = lipgloss.Color("#334155")
	danger       = lipgloss.Color("#FB7185")
	white        = lipgloss.Color("#F8FAFC")
	userGray     = lipgloss.Color("#374151")

	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(white).Background(accent).Padding(0, 1)
	statusStyle = lipgloss.NewStyle().Foreground(muted)
	errorStyle  = lipgloss.NewStyle().Foreground(danger)
	userStyle   = lipgloss.NewStyle().Foreground(white).Background(userGray).Padding(0, 1)
	inputStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(panel).Padding(0, 1)
)

const (
	enableAlternateScroll  = "\x1b[?1007h"
	disableAlternateScroll = "\x1b[?1007l"
)

func Run(ctx context.Context, modelProvider provider.Provider, options Options, input io.Reader, output io.Writer) error {
	if options.Timeout <= 0 {
		return errors.New("TUI timeout must be greater than zero")
	}
	if options.AlternateScreen {
		if _, err := io.WriteString(output, enableAlternateScroll); err != nil {
			return fmt.Errorf("enable terminal alternate scrolling: %w", err)
		}
		defer func() { _, _ = io.WriteString(output, disableAlternateScroll) }()
	}
	program := tea.NewProgram(newModel(ctx, modelProvider, options), tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output), tea.WithoutSignalHandler())
	_, err := program.Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

func newModel(ctx context.Context, modelProvider provider.Provider, options Options) model {
	applyTheme(options.Theme)
	input := textarea.New()
	input.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "❯ "
		}
		return "  "
	})
	input.Placeholder = "Ask anything or type /help…"
	input.CharLimit = 64 * 1024
	input.ShowLineNumbers = false
	input.DynamicHeight = true
	input.MinHeight = 1
	input.MaxHeight = 6
	input.MaxContentHeight = 10_000
	styles := input.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Bold(true).Foreground(accentBright)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(muted)
	styles.Cursor.Color = accentBright
	input.SetStyles(styles)
	initialFocus := input.Focus()

	chatViewport := viewport.New(viewport.WithWidth(80), viewport.WithHeight(18))
	chatViewport.SoftWrap, chatViewport.FillHeight = true, true
	activity := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle().Foreground(accentBright)))

	registry := options.Tools
	if registry == nil {
		registry, _ = tool.NewRegistry()
	}
	var runner *agent.Runner
	if modelProvider != nil {
		runner, _ = agent.New(modelProvider, registry, agent.Options{
			Model: options.Model, Instructions: options.Instructions, Stream: options.Stream,
			MaxTurns: options.MaxTurns, Approval: options.Approval,
			ContextWindowTokens: options.ContextWindowTokens, AutoCompactTokens: options.AutoCompactTokens,
			UsableContextTokens: options.UsableContextTokens,
			ToolOutputTokens:    options.ToolOutputTokens, OnUsage: func(usage provider.Usage) {
				if options.TaskState != nil {
					options.TaskState.RecordUsage(usage)
				}
			}, Hook: options.Hook, FallbackModels: options.FallbackModels,
			RequestTimeout:  options.Timeout,
			Policy:          options.Policy,
			ReasoningEffort: options.ReasoningEffort,
			ServiceTier:     options.ServiceTier,
			OnEvent:         options.OnEvent,
			OnMemoryCitation: func(ids []string) {
				if options.Memory != nil {
					options.Memory.RecordUsage(ids)
				}
			},
			Permissions: options.Permissions, ApprovalCategories: options.ApprovalCategories,
			ModelCatalog: options.ModelCatalog,
		})
	}
	currentSession := options.Session
	if currentSession.ID == "" && options.SessionStore != nil {
		currentSession, _ = options.SessionStore.New(options.Workspace, options.Model)
	}
	mode := prompts.NormalizeMode(currentSession.Mode)
	result := model{
		ctx: ctx, runner: runner, options: options, input: input, viewport: chatViewport,
		spinner: activity, initialFocus: initialFocus, initialPrompt: strings.TrimSpace(options.InitialPrompt),
		store: options.SessionStore, session: currentSession, skills: options.Skills, memory: options.Memory,
		taskState: options.TaskState, showPlan: true, planMode: mode == prompts.ModePlan, collaborationMode: mode,
		collaboration: options.Collaboration,
		draftImages:   append([]provider.Image(nil), options.InitialImages...), draftImageLabels: append([]string(nil), options.InitialImageLabels...),
	}
	if options.Collaboration != nil {
		_ = options.Collaboration.Restore(currentSession.Agents)
	}
	if len(currentSession.Messages) > 0 {
		result.loadHistory(currentSession.Messages)
	}
	return result
}

func (m model) Init() tea.Cmd {
	commands := []tea.Cmd{m.initialFocus}
	if m.initialPrompt != "" {
		prompt := m.initialPrompt
		commands = append(commands, func() tea.Msg { return initialPromptMsg(prompt) })
	}
	if m.options.UserInput != nil {
		commands = append(commands, nextUserInput(m.options.UserInput.Requests()))
	}
	return tea.Batch(commands...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case initialPromptMsg:
		if m.initialPromptFired {
			return m, nil
		}
		m.initialPromptFired = true
		return m.submit(string(msg))
	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var command tea.Cmd
		m.spinner, command = m.spinner.Update(msg)
		m.refreshRunningToolCells()
		return m, command
	case agentEventMsg:
		if msg.events != m.agentEvents {
			// A cancelled turn may finish after the composer has already started a
			// new one. Never let a stale event take ownership of the new stream.
			return m, nil
		}
		if !msg.ok {
			return m.finish(nil)
		}
		m.agentEvents = msg.events
		return m.handleAgentEvent(msg.event)
	case userInputMsg:
		m.pendingUserInput = msg.request
		m.userInputQuestion, m.userInputChoice = 0, 0
		m.userInputAnswers = make(map[string]string)
		m.userInputCustom = false
		m.userInputDraft = m.input.Value()
		m.input.Blur()
		m.resize(m.width, m.height)
		return m, nil
	case editorFinishedMsg:
		if msg.err != nil {
			m.addError("Editor failed: " + msg.err.Error())
		} else {
			m.input.SetValue(msg.content)
			m.input.MoveToEnd()
		}
		m.resize(m.width, m.height)
		return m, m.input.Focus()
	case tea.PasteMsg:
		if m.pendingApproval != nil || m.pendingUserInput != nil || m.showSessionPicker || m.showModelPicker || m.showSkillPicker {
			return m, nil
		}
		if collapsePaste(msg.Content) {
			m.draftPastes = append(m.draftPastes, msg.Content)
			m.resize(m.width, m.height)
			return m, m.input.Focus()
		}
		var command tea.Cmd
		m.input, command = m.input.Update(msg)
		m.resize(m.width, m.height)
		return m, command
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "ctrl+q" {
			if m.cancelCurrentRequest != nil {
				m.cancelCurrentRequest()
			}
			return m, tea.Quit
		}
		if m.pendingApproval != nil {
			choices := approvalChoices(m.pendingApproval)
			switch msg.String() {
			case "up", "k", "shift+tab":
				m.approvalChoice = (m.approvalChoice - 1 + len(choices)) % len(choices)
				return m, nil
			case "down", "j", "tab":
				m.approvalChoice = (m.approvalChoice + 1) % len(choices)
				return m, nil
			case "y":
				m.pendingApproval.DecideWithScope(agent.ApprovalAllowOnce)
				return m.continueAfterApproval()
			case "a":
				m.pendingApproval.DecideWithScope(agent.ApprovalAllowSession)
				return m.continueAfterApproval()
			case "p":
				if m.pendingApproval.Prefix != "" {
					m.pendingApproval.DecideWithScope(agent.ApprovalAllowPrefix)
					return m.continueAfterApproval()
				}
				return m, nil
			case "r":
				if m.pendingApproval.Prefix != "" && m.pendingApproval.PolicyPath != "" {
					m.pendingApproval.DecideWithScope(agent.ApprovalAllowPersistentPrefix)
					return m.continueAfterApproval()
				}
				return m, nil
			case "n", "esc":
				m.pendingApproval.DecideWithScope(agent.ApprovalDeny)
				return m.continueAfterApproval()
			case "enter":
				m.pendingApproval.DecideWithScope(choices[m.approvalChoice].decision)
				return m.continueAfterApproval()
			default:
				// Approval is a focused selection surface. Never leak typing into
				// the chat composer while it is open.
				return m, nil
			}
		}
		if m.pendingUserInput != nil {
			return m.updateUserInput(msg)
		}
		if m.showSessionPicker {
			return m.updateSessionPicker(msg)
		}
		if m.showModelPicker {
			return m.updateModelPicker(msg)
		}
		if m.showSkillPicker {
			return m.updateSkillPicker(msg)
		}
		if m.busy && msg.String() == "esc" {
			return m.stopCurrentTurn()
		}
		if m.commandMenuVisible() {
			switch msg.String() {
			case "esc":
				m.commandMenuDismissed = true
				m.resize(m.width, m.height)
				return m, m.input.Focus()
			case "up", "shift+tab":
				matches := matchingSlashCommands(m.input.Value())
				m.commandChoice = (m.commandChoice - 1 + len(matches)) % len(matches)
				return m, nil
			case "down":
				matches := matchingSlashCommands(m.input.Value())
				m.commandChoice = (m.commandChoice + 1) % len(matches)
				return m, nil
			case "tab":
				if selected, ok := m.selectedSlashCommand(); ok {
					m.input.SetValue(selected.name)
					m.input.MoveToEnd()
					m.commandChoice = 0
					m.resize(m.width, m.height)
				}
				return m, m.input.Focus()
			case "enter":
				selected, ok := m.selectedSlashCommand()
				if ok && m.input.Value() != selected.name {
					m.input.SetValue(selected.name)
					m.input.MoveToEnd()
					m.commandChoice = 0
					m.resize(m.width, m.height)
					return m, m.input.Focus()
				}
			}
		}
		if (m.showHelp || m.showRawTranscript) && msg.String() == "esc" {
			m.showHelp = false
			m.showRawTranscript = false
			m.refreshMessages(true)
			return m, m.input.Focus()
		}
		if (msg.String() == "backspace" || msg.String() == "delete") && m.input.Value() == "" && len(m.draftPastes) > 0 {
			m.draftPastes = m.draftPastes[:len(m.draftPastes)-1]
			m.resize(m.width, m.height)
			return m, m.input.Focus()
		}
		if strings.EqualFold(m.options.Keymap, "vim") {
			if msg.String() == "esc" {
				m.vimNormal = true
				return m, nil
			}
			if m.vimNormal {
				return m.updateVimNormal(msg)
			}
		}
		// Keep viewport paging explicit while the composer is focused. Forwarding
		// every key to viewport.DefaultKeyMap also treats ordinary b/f/u/d input as
		// pager commands and can hand repeated PgUp back to terminal scrollback.
		switch msg.String() {
		case "pgup":
			m.viewport.PageUp()
			return m, nil
		case "pgdown":
			m.viewport.PageDown()
			return m, nil
		case "up":
			// With DEC alternate-scroll enabled, a terminal converts wheel-up
			// events to cursor-up keys while mouse reporting remains disabled.
			// Reserve those keys for the transcript only when no draft is being
			// edited, so ordinary multiline cursor movement still works.
			if m.input.Value() == "" && !m.viewport.AtTop() {
				m.viewport.ScrollUp(3)
				return m, nil
			}
		case "down":
			if m.input.Value() == "" && !m.viewport.AtBottom() {
				m.viewport.ScrollDown(3)
				return m, nil
			}
		}
		if msg.String() == "shift+enter" || msg.String() == "alt+enter" || msg.String() == "ctrl+j" {
			m.input.InsertString("\n")
			m.resize(m.width, m.height)
			return m, m.input.Focus()
		}
		if msg.String() == "ctrl+g" {
			command, err := editDraftCommand(m.input.Value())
			if err != nil {
				m.addError(err.Error())
				return m, nil
			}
			m.input.Blur()
			return m, command
		}
		if msg.String() == "enter" {
			prompt := strings.TrimSpace(m.input.Value())
			if prompt == "" && len(m.draftPastes) == 0 {
				return m, nil
			}
			m.input.Reset()
			m.commandMenuDismissed = false
			m.commandChoice = 0
			if !strings.EqualFold(prompt, "/help") && !strings.EqualFold(prompt, "/raw") {
				m.showHelp = false
				m.showRawTranscript = false
			}
			m.resize(m.width, m.height)
			if m.busy {
				if prompt == "/queue" {
					m.addStatus(m.queueSummary())
					return m, nil
				}
				queuedPrompt, queuedDisplay := m.promptWithPastes(prompt)
				if m.activeRun != nil && m.activeRun.Queue(queuedPrompt) {
					m.queuedMessages = append(m.queuedMessages, queuedDisplay)
					m.draftPastes = nil
					m.input.Placeholder = "Message queued; type another follow-up…"
					m.resize(m.width, m.height)
					return m, m.input.Focus()
				}
				m.addError("The active-turn message queue is full.")
				return m, nil
			}
			if strings.HasPrefix(prompt, "/") {
				return m.command(prompt)
			}
			return m.submit(prompt)
		}
	}

	beforeMenuHeight := m.commandMenuHeight()
	beforeInputHeight := m.input.Height()
	beforeInput := m.input.Value()
	var commands []tea.Cmd
	var command tea.Cmd
	// Mouse reporting remains disabled. In alternate-screen mode the terminal's
	// alternate-scroll mode translates wheel events into the keys handled above.
	if _, isKey := msg.(tea.KeyPressMsg); !isKey {
		m.viewport, command = m.viewport.Update(msg)
		commands = append(commands, command)
	}
	m.input, command = m.input.Update(msg)
	commands = append(commands, command)
	if m.input.Value() != beforeInput {
		m.commandMenuDismissed = false
		m.commandChoice = 0
	}
	if beforeMenuHeight != m.commandMenuHeight() || beforeInputHeight != m.input.Height() {
		m.resize(m.width, m.height)
	}
	return m, tea.Batch(commands...)
}

func (m model) View() tea.View {
	width := max(20, m.width)
	sessionID := "new"
	if m.session.ID != "" {
		sessionID = shortID(m.session.ID)
	}
	mode := strings.ToUpper(string(prompts.NormalizeMode(string(m.collaborationMode))))
	header := headerStyle.Width(width).Render(fmt.Sprintf("SUPERCODE   %s   %s   %s", m.options.Model, mode, sessionID))
	m.input.SetWidth(max(8, width-6))
	inputPanel := m.renderComposer(width)
	footer := "Enter send  ·  Shift+Enter newline  ·  Ctrl+G editor  ·  /help commands"
	if strings.EqualFold(m.options.Keymap, "vim") && m.vimNormal {
		footer = "VIM NORMAL  ·  i insert  ·  h/j/k/l move  ·  x delete  ·  /help commands"
	}
	if m.pendingApproval != nil {
		footer = "↑/↓ select  ·  Enter confirm  ·  y once  ·  a tool/session  ·  p prefix/session  ·  r remember"
	} else if m.pendingUserInput != nil {
		footer = "↑/↓ select  ·  Enter confirm  ·  o custom answer  ·  Esc cancel"
	} else if m.commandMenuVisible() {
		footer = "↑/↓ select  ·  Tab complete  ·  Enter choose/run  ·  Esc close"
	} else if m.showSessionPicker {
		footer = "Type to search  ·  ↑/↓ select  ·  Enter resume  ·  Tab archived  ·  Esc close"
	} else if m.showModelPicker {
		footer = "Type to search  ·  ↑/↓ select  ·  Enter choose model  ·  Esc close"
	} else if m.showSkillPicker {
		footer = "Type to search  ·  ↑/↓ select  ·  Enter insert skill  ·  Esc close"
	} else if m.showHelp || m.showRawTranscript {
		footer = "PgUp/PgDn scroll  ·  Esc close view  ·  Type / to run a command"
	} else if m.busy {
		footer = m.spinner.View() + " Working…  ·  Esc stop  ·  Enter queue guidance for the next tool boundary"
	}
	footer = statusStyle.Width(width).PaddingLeft(1).Render(footer)
	parts := []string{header}
	if plan := m.renderPlan(width); plan != "" {
		parts = append(parts, plan)
	}
	parts = append(parts, m.viewport.View())
	if attachments := m.renderAttachments(width); attachments != "" {
		parts = append(parts, attachments)
	}
	if queued := m.renderQueued(width); queued != "" {
		parts = append(parts, queued)
	}
	if approval := m.renderApproval(width); approval != "" {
		parts = append(parts, approval)
	}
	if picker := m.renderSessionPicker(width); picker != "" {
		parts = append(parts, picker)
	}
	if picker := m.renderModelPicker(width); picker != "" {
		parts = append(parts, picker)
	}
	if picker := m.renderSkillPicker(width); picker != "" {
		parts = append(parts, picker)
	}
	if question := m.renderUserInput(width); question != "" {
		parts = append(parts, question)
	}
	if commands := m.renderCommandMenu(width); commands != "" {
		parts = append(parts, commands)
	}
	parts = append(parts, inputPanel, footer)
	view := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, parts...))
	title := defaultString(m.options.TerminalTitle, "SuperCode")
	// Never enable mouse reporting: terminals otherwise send drag events to the
	// application and ordinary text selection/copy stops working. Alternate
	// screen still keeps the TUI isolated from the shell page; PgUp/PgDn provide
	// deterministic in-app navigation.
	view.AltScreen, view.MouseMode, view.WindowTitle = m.options.AlternateScreen, tea.MouseModeNone, title
	return view
}

func (m *model) resize(width, height int) {
	m.width, m.height = max(20, width), max(8, height)
	m.viewport.SetWidth(m.width)
	m.input.SetWidth(max(8, m.width-6))
	m.viewport.SetHeight(max(3, m.height-4-m.input.Height()-m.composerPasteHeight()-m.planHeight()-m.attachmentHeight()-m.queueHeight()-m.approvalHeight()-m.sessionPickerHeight()-m.modelPickerHeight()-m.skillPickerHeight()-m.userInputHeight()-m.commandMenuHeight()))
	m.refreshMessages(false)
}
