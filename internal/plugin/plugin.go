// Package plugin loads trusted local extension bundles. A plugin may contribute
// MCP servers, hooks, instructions, and skills without changing SuperCode.
package plugin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daemon365/supercode/internal/config"
	"gopkg.in/yaml.v3"
)

type Manifest struct {
	Name         string                      `yaml:"name"`
	Version      string                      `yaml:"version,omitempty"`
	Enabled      *bool                       `yaml:"enabled,omitempty"`
	Instructions string                      `yaml:"instructions,omitempty"`
	Skills       string                      `yaml:"skills,omitempty"`
	MCPServers   map[string]config.MCPServer `yaml:"mcp_servers,omitempty"`
	Hooks        map[string][]config.Hook    `yaml:"hooks,omitempty"`
}

type Bundle struct {
	Names      []string
	Overlay    config.File
	SkillRoots []string
}

func Discover(roots ...string) (Bundle, error) {
	var bundle Bundle
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Bundle{}, fmt.Errorf("read plugin directory %s: %w", root, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
				continue
			}
			directory := filepath.Join(root, entry.Name())
			manifestPath := filepath.Join(directory, "plugin.yaml")
			manifest, err := loadManifest(manifestPath)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return Bundle{}, err
			}
			if manifest.Enabled != nil && !*manifest.Enabled {
				continue
			}
			bundle.Names = append(bundle.Names, manifest.Name)
			bundle.Overlay = config.Merge(bundle.Overlay, config.File{
				Instructions: manifest.Instructions, MCPServers: manifest.MCPServers, Hooks: manifest.Hooks,
			})
			if manifest.Skills != "" {
				skillRoot, err := contained(directory, manifest.Skills)
				if err != nil {
					return Bundle{}, fmt.Errorf("plugin %s skills: %w", manifest.Name, err)
				}
				bundle.SkillRoots = append(bundle.SkillRoots, skillRoot)
			} else {
				bundle.SkillRoots = append(bundle.SkillRoots, filepath.Join(directory, "skills"))
			}
		}
	}
	sort.Strings(bundle.Names)
	return bundle, nil
}

func loadManifest(path string) (Manifest, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Manifest{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Manifest{}, errors.New("plugin manifest must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer file.Close()
	var result Manifest
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return Manifest{}, fmt.Errorf("decode plugin manifest %s: %w", path, err)
	}
	result.Name = strings.TrimSpace(result.Name)
	if result.Name == "" {
		return Manifest{}, fmt.Errorf("plugin manifest %s has no name", path)
	}
	return result, nil
}

func LoadManifest(path string) (Manifest, error) { return loadManifest(path) }

func SaveManifest(path string, value Manifest) error {
	content, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".plugin-*.yaml")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
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
	return os.Rename(name, path)
}

func contained(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("path must be relative to the plugin")
	}
	root, _ = filepath.Abs(root)
	value, _ := filepath.Abs(filepath.Join(root, relative))
	path, err := filepath.Rel(root, value)
	if err != nil || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the plugin")
	}
	return value, nil
}
