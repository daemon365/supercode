// Package permission models explicit, scoped access grants independently from
// the approval UI and the operating-system sandbox implementation.
package permission

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type Scope string

const (
	ScopeTurn    Scope = "turn"
	ScopeSession Scope = "session"
)

type FileSystem struct {
	Read  []string `json:"read,omitempty" yaml:"read,omitempty"`
	Write []string `json:"write,omitempty" yaml:"write,omitempty"`
}

type Network struct {
	Domains   []string `json:"domains,omitempty" yaml:"domains,omitempty"`
	Protocols []string `json:"protocols,omitempty" yaml:"protocols,omitempty"`
}

type Profile struct {
	FileSystem FileSystem `json:"file_system,omitempty" yaml:"file_system,omitempty"`
	Network    Network    `json:"network,omitempty" yaml:"network,omitempty"`
}

type Request struct {
	Reason      string  `json:"reason"`
	Permissions Profile `json:"permissions"`
}

type Snapshot struct {
	Turn    Profile `json:"turn"`
	Session Profile `json:"session"`
}

type Manager struct {
	workspace string
	mu        sync.RWMutex
	turn      Profile
	session   Profile
}

func NewManager(workspace string) (*Manager, error) {
	root, err := canonicalDirectory(workspace)
	if err != nil {
		return nil, fmt.Errorf("create permission manager: %w", err)
	}
	return &Manager{workspace: root}, nil
}

// BeginTurn expires all access that was approved for only the previous user
// turn. Session grants remain active until the process exits.
func (m *Manager) BeginTurn() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.turn = Profile{}
	m.mu.Unlock()
}

func (m *Manager) Grant(profile Profile, scope Scope) (Profile, error) {
	if m == nil {
		return Profile{}, errors.New("permission manager is unavailable")
	}
	normalized, err := normalize(profile)
	if err != nil {
		return Profile{}, err
	}
	if Empty(normalized) {
		return Profile{}, errors.New("at least one permission is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch scope {
	case ScopeTurn:
		m.turn = merge(m.turn, normalized)
	case ScopeSession:
		m.session = merge(m.session, normalized)
	default:
		return Profile{}, fmt.Errorf("unsupported permission scope %q", scope)
	}
	return normalized, nil
}

func (m *Manager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Snapshot{Turn: clone(m.turn), Session: clone(m.session)}
}

func (m *Manager) ReadRoots() []string {
	snapshot := m.Snapshot()
	return uniqueSorted(append(append([]string(nil), snapshot.Session.FileSystem.Read...), append(snapshot.Turn.FileSystem.Read, append(snapshot.Session.FileSystem.Write, snapshot.Turn.FileSystem.Write...)...)...))
}

func (m *Manager) WriteRoots() []string {
	snapshot := m.Snapshot()
	return uniqueSorted(append(append([]string(nil), snapshot.Session.FileSystem.Write...), snapshot.Turn.FileSystem.Write...))
}

func (m *Manager) AllowsURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	return m.AllowsNetwork(parsed.Scheme, parsed.Hostname())
}

func (m *Manager) AllowsNetwork(protocol, domain string) bool {
	if m == nil {
		return false
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	snapshot := m.Snapshot()
	combined := merge(snapshot.Session, snapshot.Turn).Network
	protocolAllowed := contains(combined.Protocols, "*") || contains(combined.Protocols, protocol)
	domainAllowed := contains(combined.Domains, "*")
	for _, allowed := range combined.Domains {
		if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
			domainAllowed = true
			break
		}
	}
	return protocolAllowed && domainAllowed
}

// AllowsUnrestrictedNetwork is deliberately strict. The shell sandbox is only
// opened when both protocol and domain were explicitly granted as wildcards;
// domain-specific grants remain enforceable only by URL-aware tools.
func (m *Manager) AllowsUnrestrictedNetwork() bool {
	return m != nil && m.AllowsNetwork("*", "*")
}

func Empty(profile Profile) bool {
	return len(profile.FileSystem.Read) == 0 && len(profile.FileSystem.Write) == 0 && len(profile.Network.Domains) == 0 && len(profile.Network.Protocols) == 0
}

func normalize(profile Profile) (Profile, error) {
	var result Profile
	for _, path := range profile.FileSystem.Read {
		resolved, err := canonicalDirectory(path)
		if err != nil {
			return Profile{}, fmt.Errorf("read permission %q: %w", path, err)
		}
		result.FileSystem.Read = append(result.FileSystem.Read, resolved)
	}
	for _, path := range profile.FileSystem.Write {
		resolved, err := canonicalDirectory(path)
		if err != nil {
			return Profile{}, fmt.Errorf("write permission %q: %w", path, err)
		}
		result.FileSystem.Write = append(result.FileSystem.Write, resolved)
	}
	for _, value := range profile.Network.Protocols {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "*" && value != "http" && value != "https" {
			return Profile{}, fmt.Errorf("unsupported network protocol %q", value)
		}
		result.Network.Protocols = append(result.Network.Protocols, value)
	}
	for _, value := range profile.Network.Domains {
		value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		if value == "" || strings.ContainsAny(value, "/:@ \\") {
			return Profile{}, fmt.Errorf("invalid network domain %q", value)
		}
		result.Network.Domains = append(result.Network.Domains, value)
	}
	result.FileSystem.Read = uniqueSorted(result.FileSystem.Read)
	result.FileSystem.Write = uniqueSorted(result.FileSystem.Write)
	result.Network.Domains = uniqueSorted(result.Network.Domains)
	result.Network.Protocols = uniqueSorted(result.Network.Protocols)
	return result, nil
}

func canonicalDirectory(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("path is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("permission roots must be existing directories")
	}
	return filepath.Clean(resolved), nil
}

func merge(left, right Profile) Profile {
	return Profile{
		FileSystem: FileSystem{
			Read:  uniqueSorted(append(append([]string(nil), left.FileSystem.Read...), right.FileSystem.Read...)),
			Write: uniqueSorted(append(append([]string(nil), left.FileSystem.Write...), right.FileSystem.Write...)),
		},
		Network: Network{
			Domains:   uniqueSorted(append(append([]string(nil), left.Network.Domains...), right.Network.Domains...)),
			Protocols: uniqueSorted(append(append([]string(nil), left.Network.Protocols...), right.Network.Protocols...)),
		},
	}
}

func clone(value Profile) Profile { return merge(Profile{}, value) }

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
