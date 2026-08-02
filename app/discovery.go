package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/hook"
	projectinstructions "github.com/daemon365/supercode/internal/instructions"
	"github.com/daemon365/supercode/internal/memory"
	"github.com/daemon365/supercode/internal/plugin"
	"github.com/daemon365/supercode/internal/policy"
	"github.com/daemon365/supercode/internal/session"
	"github.com/daemon365/supercode/internal/skill"
)

type projectEnvironment struct {
	fileConfig        config.File
	options           options
	plugins           plugin.Bundle
	agentInstructions string
	configSummary     string
}

func discoverProjectEnvironment(startup startupState, configDirectory string, policyStore *policy.Store) (projectEnvironment, error) {
	fileConfig, options := startup.fileConfig, startup.options
	pluginRoots := []string{filepath.Join(configDirectory, "plugins")}
	if config.IsWorkspaceTrusted(fileConfig, options.workspace) {
		pluginRoots = append(pluginRoots, filepath.Join(options.workspace, ".supercode", "plugins"))
	}
	plugins, err := plugin.Discover(pluginRoots...)
	if err != nil {
		return projectEnvironment{}, err
	}
	fileConfig = config.Merge(fileConfig, plugins.Overlay)
	if strings.TrimSpace(plugins.Overlay.Instructions) != "" {
		options.instructions = strings.TrimSpace(options.instructions + "\n\n" + plugins.Overlay.Instructions)
	}
	instructionSet, err := projectinstructions.Discover(projectinstructions.Options{
		Root:          projectinstructions.FindProjectRoot(options.workspace),
		CWD:           options.workspace,
		FallbackNames: fileConfig.ProjectDocFallbacks,
		MaxBytes:      fileConfig.ProjectDocMaxBytes,
	})
	if err != nil {
		return projectEnvironment{}, err
	}
	instructionSources := strings.Join(instructionSet.Sources(), "\n  - ")
	if instructionSources == "" {
		instructionSources = "none"
	} else {
		instructionSources = "\n  - " + instructionSources
	}
	configSummary := fmt.Sprintf(
		"User config: %s\nProject config: %s — %s\nProject trusted: %t\nPlugins: %s\nPrecedence: CLI > environment > trusted project > user > defaults",
		startup.configPath, startup.projectConfigPath, startup.projectConfigStatus,
		config.IsWorkspaceTrusted(fileConfig, options.workspace), strings.Join(plugins.Names, ", "),
	)
	configSummary += "\nInstruction files: " + instructionSources
	configSummary += fmt.Sprintf("\nPolicy file: %s (%d rules)", policyStore.Path(), len(policyStore.List()))
	if len(plugins.Names) == 0 {
		configSummary = strings.Replace(configSummary, "Plugins: \n", "Plugins: none\n", 1)
	}
	return projectEnvironment{
		fileConfig: fileConfig, options: options, plugins: plugins,
		agentInstructions: codingInstructionsFromSet(options.workspace, options.instructions, instructionSet),
		configSummary:     configSummary,
	}, nil
}

type applicationStores struct {
	sessions   *session.Store
	skills     *skill.Catalog
	skillRoots []string
	memory     *memory.Store
	hooks      *hook.Manager
}

func openApplicationStores(ctx context.Context, configDirectory string, environment projectEnvironment) (applicationStores, error) {
	sessionStore, err := session.NewStore(filepath.Join(configDirectory, "sessions"))
	if err != nil {
		return applicationStores{}, err
	}
	userSkills := filepath.Join(configDirectory, "skills")
	if err := os.MkdirAll(userSkills, 0o700); err != nil {
		return applicationStores{}, fmt.Errorf("create skills directory: %w", err)
	}
	skillRoots := append([]string{userSkills}, environment.plugins.SkillRoots...)
	if config.IsWorkspaceTrusted(environment.fileConfig, environment.options.workspace) {
		skillRoots = append(skillRoots, filepath.Join(environment.options.workspace, ".supercode", "skills"))
	}
	skills, err := skill.Discover(skillRoots...)
	if err != nil {
		return applicationStores{}, fmt.Errorf("discover skills: %w", err)
	}
	memoryStore, err := memory.NewStore(filepath.Join(configDirectory, "memories"), filepath.Join(configDirectory, "memory.md"))
	if err != nil {
		return applicationStores{}, err
	}
	memoryStore.ConfigureAdvanced(applyMemoryConfig(memory.DefaultConfig(), environment.fileConfig))
	hookManager, err := hook.New(environment.options.workspace, environment.fileConfig.Hooks)
	if err != nil {
		return applicationStores{}, err
	}
	if err := hookManager.Session(ctx, "session_start"); err != nil {
		return applicationStores{}, err
	}
	return applicationStores{sessions: sessionStore, skills: skills, skillRoots: skillRoots, memory: memoryStore, hooks: hookManager}, nil
}

func (s applicationStores) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.memory != nil {
		_ = s.memory.StopStartup(ctx)
	}
	if s.hooks != nil {
		_ = s.hooks.Session(ctx, "session_end")
	}
}
