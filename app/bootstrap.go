package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/daemon365/supercode/internal/attachment"
	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/provider"
)

type startupState struct {
	configPath          string
	fileConfig          config.File
	userConfig          config.File
	projectConfigPath   string
	projectConfigStatus string
	options             options
	promptArgs          []string
	initialImages       []provider.Image
}

func prepareStartup(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) (startupState, bool, error) {
	configPath, err := config.Path(lookupEnv)
	if err != nil {
		return startupState{}, false, err
	}
	if _, err := config.Ensure(configPath); err != nil {
		return startupState{}, false, fmt.Errorf("initialize config: %w", err)
	}
	fileConfig, err := config.Load(configPath)
	if err != nil {
		return startupState{}, false, err
	}
	options, promptArgs, err := parseOptionsWithConfig(args, stderr, lookupEnv, fileConfig)
	if err != nil {
		return startupState{}, false, err
	}
	if options.helpShown {
		return startupState{}, true, nil
	}
	if options.initConfig {
		_, err := fmt.Fprintln(stdout, configPath)
		return startupState{}, true, err
	}
	if options.workspace == "" {
		options.workspace, err = os.Getwd()
		if err != nil {
			return startupState{}, false, fmt.Errorf("find workspace: %w", err)
		}
	}
	options.workspace, err = filepath.Abs(options.workspace)
	if err != nil {
		return startupState{}, false, fmt.Errorf("resolve workspace: %w", err)
	}
	workspaceInfo, err := os.Stat(options.workspace)
	if err != nil {
		return startupState{}, false, fmt.Errorf("inspect workspace: %w", err)
	}
	if !workspaceInfo.IsDir() {
		return startupState{}, false, errors.New("workspace must be a directory")
	}
	initialImages := make([]provider.Image, 0, len(options.imagePaths))
	for _, path := range options.imagePaths {
		image, loadErr := attachment.Load(path, "high")
		if loadErr != nil {
			return startupState{}, false, fmt.Errorf("load image %s: %w", path, loadErr)
		}
		initialImages = append(initialImages, image)
	}
	if options.trustProject {
		if err := config.TrustWorkspace(configPath, options.workspace); err != nil {
			return startupState{}, false, fmt.Errorf("trust project: %w", err)
		}
		fileConfig, err = config.Load(configPath)
		if err != nil {
			return startupState{}, false, err
		}
	}
	userConfig := fileConfig
	projectConfigPath := config.ProjectPath(options.workspace)
	projectConfigStatus := "not found"
	if _, statErr := os.Stat(projectConfigPath); statErr == nil {
		if config.IsWorkspaceTrusted(fileConfig, options.workspace) {
			projectConfigStatus = "loaded (trusted)"
			projectConfig, loadErr := config.Load(projectConfigPath)
			if loadErr != nil {
				return startupState{}, false, fmt.Errorf("load trusted project config: %w", loadErr)
			}
			fileConfig = config.Merge(fileConfig, projectConfig)
			mergedOptions, mergedPromptArgs, parseErr := parseOptionsWithConfig(args, stderr, lookupEnv, fileConfig)
			if parseErr != nil {
				return startupState{}, false, parseErr
			}
			mergedOptions.workspace = options.workspace
			options, promptArgs = mergedOptions, mergedPromptArgs
		} else {
			projectConfigStatus = "ignored (untrusted)"
			_, _ = fmt.Fprintf(stderr, "Ignoring untrusted project config %s; run with --trust-project to enable it.\n", projectConfigPath)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return startupState{}, false, fmt.Errorf("inspect project config: %w", statErr)
	}
	return startupState{
		configPath: configPath, fileConfig: fileConfig, userConfig: userConfig,
		projectConfigPath: projectConfigPath, projectConfigStatus: projectConfigStatus,
		options: options, promptArgs: promptArgs, initialImages: initialImages,
	}, false, nil
}
