package tool

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"strings"

	"github.com/daemon365/supercode/internal/provider"
)

type viewImageTool struct{ workspace workspace }

func (*viewImageTool) Definition() provider.ToolDefinition {
	return definition("view_image", "Load a local PNG, JPEG, or GIF from the workspace and return it to an image-capable model for visual inspection.", `{"type":"object","properties":{"path":{"type":"string"},"detail":{"type":"string","enum":["high","original"]}},"required":["path"],"additionalProperties":false}`)
}
func (*viewImageTool) Risk(string) Risk { return RiskRead }
func (*viewImageTool) Summary(arguments string) string {
	return argumentSummary("view image", arguments)
}
func (t *viewImageTool) Execute(_ context.Context, arguments string) (Result, error) {
	var input struct{ Path, Detail string }
	var raw struct {
		Path   string `json:"path"`
		Detail string `json:"detail"`
	}
	if err := decodeArguments(arguments, &raw); err != nil {
		return Result{}, err
	}
	input.Path, input.Detail = raw.Path, raw.Detail
	if input.Detail == "" {
		input.Detail = "high"
	}
	if input.Detail != "high" && input.Detail != "original" {
		return Result{}, errors.New("detail must be high or original")
	}
	path, err := t.workspace.resolve(input.Path, false)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Result{}, err
	}
	if !info.Mode().IsRegular() {
		return Result{}, errors.New("image path is not a regular file")
	}
	if info.Size() > 20*1024*1024 {
		return Result{}, errors.New("image exceeds the 20 MiB limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	config, format, err := image.DecodeConfig(file)
	_ = file.Close()
	if err != nil {
		return Result{}, fmt.Errorf("decode image: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/" + format
	}
	return Result{Content: fmt.Sprintf("Loaded %s (%s, %dx%d, %d bytes).", t.workspace.display(path), mime, config.Width, config.Height, len(data)), Images: []provider.Image{{MIMEType: mime, Data: base64.StdEncoding.EncodeToString(data), Detail: input.Detail}}}, nil
}
