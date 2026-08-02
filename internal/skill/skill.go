// Package skill discovers SKILL.md instructions and injects explicitly named
// skills into a model turn.
package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxSkills    = 64
	maxSkillSize = 64 * 1024
)

var mentionPattern = regexp.MustCompile(`(?:^|\s)\$([A-Za-z0-9_-]+)\b`)

type Skill struct {
	Name        string
	Description string
	Path        string
	Content     string
	Requires    []string
	Triggers    []string
}

type Catalog struct {
	skills map[string]Skill
	roots  []string
}

// Discover reads SKILL.md files below each root. Later roots override an
// earlier skill with the same name, so project skills can override user skills.
func Discover(roots ...string) (*Catalog, error) {
	catalog := &Catalog{skills: make(map[string]Skill), roots: append([]string(nil), roots...)}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		info, err := os.Stat(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			continue
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if len(catalog.skills) >= maxSkills {
				return fs.SkipAll
			}
			if entry.IsDir() && path != root {
				relative, _ := filepath.Rel(root, path)
				if len(strings.Split(relative, string(filepath.Separator))) > 3 {
					return filepath.SkipDir
				}
			}
			if entry.IsDir() || entry.Name() != "SKILL.md" {
				return nil
			}
			loaded, err := load(path)
			if err != nil {
				return nil
			}
			catalog.skills[loaded.Name] = loaded
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

func (c *Catalog) Skills() []Skill {
	values := make([]Skill, 0, len(c.skills))
	for _, value := range c.skills {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	return values
}

func (c *Catalog) Len() int { return len(c.skills) }

func (c *Catalog) Reload() error {
	if c == nil {
		return errors.New("skill catalog is unavailable")
	}
	loaded, err := Discover(c.roots...)
	if err != nil {
		return err
	}
	c.skills = loaded.skills
	return nil
}

func (s Skill) MissingDependencies() []string {
	var missing []string
	for _, name := range s.Requires {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// Instructions returns the model-visible local skill catalog with exact file
// paths. The model reads a selected local SKILL.md through normal read-only
// filesystem tools; special skill tools are reserved for non-file authorities
// that SuperCode does not currently expose.
func (c *Catalog) Instructions(prompt string) string {
	if c == nil || len(c.skills) == 0 {
		return ""
	}
	var output strings.Builder
	output.WriteString("Available skills (mention a skill as $name to use it):\n")
	for _, value := range c.Skills() {
		fmt.Fprintf(&output, "- $%s: %s (source: %s)\n", value.Name, value.Description, value.Path)
	}
	output.WriteString("If the user names a skill with $name or plain text, or the task clearly matches its description, you must use it for this turn. The runtime already provides every local skill path above. Never search the filesystem or inspect .git directories to locate skills. If a skill is selected, use read_file on its exact path and continue reading until EOF before acting. Resolve referenced relative files from the directory containing that SKILL.md.\n")
	selected := make(map[string]bool)
	for _, match := range mentionPattern.FindAllStringSubmatch(prompt, -1) {
		name := match[1]
		_, exists := c.skills[name]
		if !exists || selected[name] {
			continue
		}
		selected[name] = true
		fmt.Fprintf(&output, "\nSelected skill $%s. Read the complete file at %s before acting.\n", name, c.skills[name].Path)
	}
	for _, value := range c.Skills() {
		if selected[value.Name] || !plainNameMentioned(prompt, value.Name) {
			continue
		}
		selected[value.Name] = true
		fmt.Fprintf(&output, "\nSelected skill %s by name. Read the complete file at %s before acting.\n", value.Name, value.Path)
	}
	for _, value := range c.Skills() {
		if selected[value.Name] || len(value.Triggers) == 0 || len(value.MissingDependencies()) > 0 {
			continue
		}
		for _, trigger := range value.Triggers {
			if strings.Contains(strings.ToLower(prompt), strings.ToLower(strings.TrimSpace(trigger))) {
				selected[value.Name] = true
				fmt.Fprintf(&output, "\nAutomatically selected skill $%s (trigger %q). Read the complete file at %s before acting.\n", value.Name, trigger, value.Path)
				break
			}
		}
	}
	return output.String()
}

func plainNameMentioned(prompt, name string) bool {
	pattern := regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(name) + `(?:$|[^A-Za-z0-9_-])`)
	return pattern.MatchString(prompt)
}

func load(path string) (Skill, error) {
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return Skill{}, err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return Skill{}, errors.New("skill file must not be a symbolic link")
	}
	info, err := os.Stat(path)
	if err != nil {
		return Skill{}, err
	}
	if info.Size() > maxSkillSize {
		return Skill{}, errors.New("skill file is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	content := string(data)
	metadataText, ok := frontmatter(content)
	if !ok {
		return Skill{}, errors.New("skill frontmatter is required")
	}
	var metadata struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Requires    []string `yaml:"requires,omitempty"`
		Triggers    []string `yaml:"triggers,omitempty"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(metadataText))
	decoder.KnownFields(true)
	if err := decoder.Decode(&metadata); err != nil {
		return Skill{}, err
	}
	metadata.Name = strings.TrimSpace(metadata.Name)
	metadata.Description = strings.TrimSpace(metadata.Description)
	if metadata.Name == "" || metadata.Description == "" {
		return Skill{}, errors.New("skill name and description are required")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(metadata.Name) {
		return Skill{}, errors.New("invalid skill name")
	}
	return Skill{Name: metadata.Name, Description: metadata.Description, Path: path, Content: content, Requires: metadata.Requires, Triggers: metadata.Triggers}, nil
}

func frontmatter(content string) (string, bool) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return "", false
	}
	end := strings.Index(normalized[4:], "\n---\n")
	if end < 0 {
		return "", false
	}
	return normalized[4 : 4+end], true
}
