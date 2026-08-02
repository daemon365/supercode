// Package policy stores explicit, user-approved execution rules.
package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daemon365/supercode/internal/provider"
	"gopkg.in/yaml.v3"
)

const Version = 1

type Rule struct {
	ID        string    `yaml:"id" json:"id"`
	Kind      string    `yaml:"kind" json:"kind"`
	Tool      string    `yaml:"tool,omitempty" json:"tool,omitempty"`
	Argv      []string  `yaml:"argv,omitempty" json:"argv,omitempty"`
	CreatedAt time.Time `yaml:"created_at" json:"created_at"`
}

type file struct {
	Version int    `yaml:"version"`
	Rules   []Rule `yaml:"rules,omitempty"`
}

type Store struct {
	path  string
	mu    sync.RWMutex
	rules []Rule
}

func NewStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("policy path is required")
	}
	store := &Store{path: path}
	if err := store.reload(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) List() []Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Rule(nil), s.rules...)
}

func (s *Store) AddCommandPrefix(prefix string) (Rule, error) {
	argv, ok := ParseCommandPrefix(prefix)
	if !ok {
		return Rule{}, errors.New("only a simple command argv prefix can be persisted")
	}
	rule := Rule{ID: ruleID("command_prefix", "exec_command", argv), Kind: "command_prefix", Tool: "exec_command", Argv: argv, CreatedAt: time.Now().UTC()}
	return s.add(rule)
}

func (s *Store) AddTool(name string) (Rule, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Rule{}, errors.New("tool name is required")
	}
	rule := Rule{ID: ruleID("tool", name, nil), Kind: "tool", Tool: name, CreatedAt: time.Now().UTC()}
	return s.add(rule)
}

func (s *Store) add(rule Rule) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.rules {
		if existing.ID == rule.ID {
			return existing, nil
		}
	}
	s.rules = append(s.rules, rule)
	sort.SliceStable(s.rules, func(left, right int) bool { return s.rules[left].ID < s.rules[right].ID })
	return rule, s.saveLocked()
}

func (s *Store) Remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, rule := range s.rules {
		if rule.ID != id {
			continue
		}
		s.rules = append(s.rules[:index], s.rules[index+1:]...)
		return true, s.saveLocked()
	}
	return false, nil
}

func (s *Store) Allows(call provider.ToolCall) (Rule, bool) {
	commandArgv := CommandArgv(call)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rule := range s.rules {
		switch rule.Kind {
		case "tool":
			if rule.Tool == call.Name {
				return rule, true
			}
		case "command_prefix":
			if (call.Name == "exec_command" || call.Name == "run_command") && hasArgvPrefix(commandArgv, rule.Argv) {
				return rule, true
			}
		}
	}
	return Rule{}, false
}

func ParseCommandPrefix(command string) ([]string, bool) {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "\n;&|><`$(){}") {
		return nil, false
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, false
	}
	limit := 1
	if len(fields) > 1 && !strings.HasPrefix(fields[1], "-") {
		limit = 2
	}
	return append([]string(nil), fields[:limit]...), true
}

func CommandArgv(call provider.ToolCall) []string {
	if call.Name != "exec_command" && call.Name != "run_command" {
		return nil
	}
	var arguments map[string]any
	if yaml.Unmarshal([]byte(call.Arguments), &arguments) != nil {
		return nil
	}
	command, _ := arguments["cmd"].(string)
	if command == "" {
		command, _ = arguments["command"].(string)
	}
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "\n;&|><`$(){}") {
		return nil
	}
	return strings.Fields(command)
}

func PrefixForCall(call provider.ToolCall) string {
	argv := CommandArgv(call)
	if len(argv) == 0 {
		return ""
	}
	limit := 1
	if len(argv) > 1 && !strings.HasPrefix(argv[1], "-") {
		limit = 2
	}
	return strings.Join(argv[:limit], " ")
}

func hasArgvPrefix(command, prefix []string) bool {
	if len(prefix) == 0 || len(command) < len(prefix) {
		return false
	}
	for index := range prefix {
		if command[index] != prefix[index] {
			return false
		}
	}
	return true
}

func ruleID(kind, tool string, argv []string) string {
	value := strings.NewReplacer("/", "_", "\\", "_", " ", "-").Replace(strings.Join(append([]string{kind, tool}, argv...), "-"))
	return strings.Trim(value, "-")
}

func (s *Store) reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	var data file
	if err := yaml.Unmarshal(content, &data); err != nil {
		return fmt.Errorf("parse policy: %w", err)
	}
	if data.Version != Version {
		return fmt.Errorf("unsupported policy version %d", data.Version)
	}
	s.rules = append([]Rule(nil), data.Rules...)
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	content, err := yaml.Marshal(file{Version: Version, Rules: s.rules})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".policy-*.yaml")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, s.path)
}
