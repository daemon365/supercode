package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/hook"
	"github.com/daemon365/supercode/internal/plugin"
	"github.com/daemon365/supercode/internal/skill"
)

func summarizeHooks(definitions map[string][]config.Hook) []string {
	events := make([]string, 0, len(definitions))
	for event := range definitions {
		events = append(events, event)
	}
	sort.Strings(events)
	var result []string
	for _, event := range events {
		for _, item := range definitions[event] {
			if item.Enabled != nil && !*item.Enabled {
				continue
			}
			trust := "unhashed"
			if item.SHA256 != "" {
				trust = "sha256:" + item.SHA256[:min(12, len(item.SHA256))]
			}
			result = append(result, event+" — "+strings.Join(item.Command, " ")+" — "+trust)
		}
	}
	return result
}

func runSkillCommand(root, action string, values []string, output io.Writer) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	switch action {
	case "list", "check":
		catalog, err := skill.Discover(root)
		if err != nil {
			return err
		}
		if catalog.Len() == 0 {
			_, err := fmt.Fprintln(output, "No user skills installed.")
			return err
		}
		for _, item := range catalog.Skills() {
			status := "ready"
			if missing := item.MissingDependencies(); len(missing) > 0 {
				status = "missing: " + strings.Join(missing, ", ")
			}
			if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", item.Name, status, item.Description, item.Path); err != nil {
				return err
			}
		}
		return nil
	case "install":
		source, err := filepath.Abs(values[0])
		if err != nil {
			return err
		}
		catalog, err := skill.Discover(source)
		if err != nil {
			return err
		}
		items := catalog.Skills()
		if len(items) != 1 {
			return fmt.Errorf("skill source must contain exactly one SKILL.md; found %d", len(items))
		}
		target, err := safeManagedChild(root, items[0].Name)
		if err != nil {
			return err
		}
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("skill %q is already installed", items[0].Name)
		}
		if err := copyDirectory(source, target); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "Installed skill "+items[0].Name+".")
		return err
	case "remove":
		target, err := safeManagedChild(root, values[0])
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
			return fmt.Errorf("installed skill %q was not found", values[0])
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "Removed skill "+values[0]+". This cannot be undone.")
		return err
	default:
		return fmt.Errorf("unknown skill action %q", action)
	}
}

func runPluginCommand(root, action string, values []string, output io.Writer) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if action == "list" {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		count := 0
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			manifest, err := plugin.LoadManifest(filepath.Join(root, entry.Name(), "plugin.yaml"))
			if err != nil {
				continue
			}
			status := "enabled"
			if manifest.Enabled != nil && !*manifest.Enabled {
				status = "disabled"
			}
			if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", entry.Name(), manifest.Name, manifest.Version, status); err != nil {
				return err
			}
			count++
		}
		if count == 0 {
			_, err = fmt.Fprintln(output, "No plugins installed.")
		}
		return err
	}
	if action == "install" {
		source, err := filepath.Abs(values[0])
		if err != nil {
			return err
		}
		if _, err := plugin.LoadManifest(filepath.Join(source, "plugin.yaml")); err != nil {
			return err
		}
		target, err := safeManagedChild(root, filepath.Base(source))
		if err != nil {
			return err
		}
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("plugin directory %q already exists", filepath.Base(source))
		}
		if err := copyDirectory(source, target); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "Installed plugin "+filepath.Base(source)+".")
		return err
	}
	target, err := safeManagedChild(root, values[0])
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(target, "plugin.yaml")
	manifest, err := plugin.LoadManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("installed plugin %q was not found: %w", values[0], err)
	}
	switch action {
	case "enable", "disable":
		enabled := action == "enable"
		manifest.Enabled = &enabled
		if err := plugin.SaveManifest(manifestPath, manifest); err != nil {
			return err
		}
		verb := "Disabled"
		if enabled {
			verb = "Enabled"
		}
		_, err = fmt.Fprintf(output, "%s plugin %s.\n", verb, values[0])
		return err
	case "remove":
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "Removed plugin "+values[0]+". This cannot be undone.")
		return err
	default:
		return fmt.Errorf("unknown plugin action %q", action)
	}
}

func runHookCommand(configPath, workspace string, configuration config.File, action string, values []string, output io.Writer) error {
	if action == "list" {
		events := make([]string, 0, len(configuration.Hooks))
		for event := range configuration.Hooks {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, event := range events {
			for index, item := range configuration.Hooks[event] {
				status := "enabled"
				if item.Enabled != nil && !*item.Enabled {
					status = "disabled"
				}
				trust := "unhashed"
				if item.SHA256 != "" {
					trust = "sha256:" + item.SHA256[:min(12, len(item.SHA256))]
				}
				if _, err := fmt.Fprintf(output, "%s\t%d\t%s\t%s\t%s\n", event, index+1, status, trust, hook.ResolveCommand(workspace, item.Command)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	event, index, err := hookTarget(configuration, values)
	if err != nil {
		return err
	}
	item := configuration.Hooks[event][index]
	switch action {
	case "enable", "disable":
		enabled := action == "enable"
		item.Enabled = &enabled
	case "trust":
		path := item.Command[0]
		if !filepath.IsAbs(path) {
			path = filepath.Join(workspace, path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		item.SHA256 = hex.EncodeToString(digest[:])
	default:
		return fmt.Errorf("unknown hook action %q", action)
	}
	configuration.Hooks[event][index] = item
	if err := config.Save(configPath, configuration); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Updated hook %s #%d.\n", event, index+1)
	return err
}

func hookTarget(configuration config.File, values []string) (string, int, error) {
	if len(values) != 2 {
		return "", 0, errors.New("hook event and index are required")
	}
	index, err := strconv.Atoi(values[1])
	if err != nil || index < 1 || index > len(configuration.Hooks[values[0]]) {
		return "", 0, fmt.Errorf("hook %s #%s was not found", values[0], values[1])
	}
	return values[0], index - 1, nil
}

func safeManagedChild(root, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return "", errors.New("managed item name must be one path segment")
	}
	return filepath.Join(root, name), nil
}

func copyDirectory(source, target string) error {
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return errors.New("source must be a directory")
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source contains symbolic link %s", entry.Name())
		}
		from, to := filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())
		if entry.IsDir() {
			if err := copyDirectory(from, to); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, content, 0o600); err != nil {
			return err
		}
	}
	return nil
}
