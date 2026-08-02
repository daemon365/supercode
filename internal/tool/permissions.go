package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/daemon365/supercode/internal/permission"
	"github.com/daemon365/supercode/internal/provider"
)

// PermissionRequester lets the agent distinguish an access grant from a
// normal tool approval and apply the selected turn/session lifetime.
type PermissionRequester interface {
	PermissionRequest(arguments string) (permission.Request, error)
}

type requestPermissionsTool struct{ manager *permission.Manager }

func newRequestPermissionsTool(manager *permission.Manager) Tool {
	return &requestPermissionsTool{manager: manager}
}

func (*requestPermissionsTool) Definition() provider.ToolDefinition {
	return definition("request_permissions", "Request additional file-system or network access. The user chooses whether a grant lasts for this turn or the current session.", `{"type":"object","properties":{"reason":{"type":"string"},"permissions":{"type":"object","properties":{"file_system":{"type":"object","properties":{"read":{"type":"array","items":{"type":"string"}},"write":{"type":"array","items":{"type":"string"}}},"additionalProperties":false},"network":{"type":"object","properties":{"domains":{"type":"array","items":{"type":"string"}},"protocols":{"type":"array","items":{"type":"string","enum":["http","https","*"]}}},"additionalProperties":false}},"additionalProperties":false}},"required":["reason","permissions"],"additionalProperties":false}`)
}

func (*requestPermissionsTool) Risk(string) Risk   { return RiskPermission }
func (*requestPermissionsTool) Category() Category { return CategoryPermission }
func (*requestPermissionsTool) Summary(arguments string) string {
	return argumentSummary("request permissions", arguments)
}

func (t *requestPermissionsTool) PermissionRequest(arguments string) (permission.Request, error) {
	var request permission.Request
	if err := decodeArguments(arguments, &request); err != nil {
		return permission.Request{}, err
	}
	if strings.TrimSpace(request.Reason) == "" {
		return permission.Request{}, errors.New("permission reason is required")
	}
	if permission.Empty(request.Permissions) {
		return permission.Request{}, errors.New("at least one permission is required")
	}
	return request, nil
}

func (t *requestPermissionsTool) Execute(_ context.Context, arguments string) (Result, error) {
	if _, err := t.PermissionRequest(arguments); err != nil {
		return Result{}, err
	}
	if t.manager == nil {
		return Result{}, errors.New("permission manager is unavailable")
	}
	encoded, err := json.MarshalIndent(t.manager.Snapshot(), "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode permission snapshot: %w", err)
	}
	return Result{Content: string(encoded)}, nil
}
