package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/daemon365/supercode/internal/attachment"
	"github.com/daemon365/supercode/internal/provider"
)

const viewImageMemoryLimit = 64 * 1024 * 1024

var viewImageMemory = newByteBudget(viewImageMemoryLimit)

type byteBudget struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	changed chan struct{}
}

func newByteBudget(limit int64) *byteBudget {
	return &byteBudget{limit: limit, changed: make(chan struct{})}
}

func (b *byteBudget) acquire(ctx context.Context, amount int64) (func(), error) {
	if amount < 0 || amount > b.limit {
		return nil, fmt.Errorf("image requires %d bytes, exceeding the %d-byte memory budget", amount, b.limit)
	}
	for {
		b.mu.Lock()
		if b.used+amount <= b.limit {
			b.used += amount
			b.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					b.mu.Lock()
					b.used -= amount
					close(b.changed)
					b.changed = make(chan struct{})
					b.mu.Unlock()
				})
			}, nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

type viewImageTool struct{ workspace workspace }

func (*viewImageTool) Definition() provider.ToolDefinition {
	return definition("view_image", "Load a local PNG, JPEG, or GIF from the workspace and return it to an image-capable model for visual inspection.", `{"type":"object","properties":{"path":{"type":"string"},"detail":{"type":"string","enum":["high","original"]}},"required":["path"],"additionalProperties":false}`)
}
func (*viewImageTool) Risk(string) Risk { return RiskRead }
func (*viewImageTool) Summary(arguments string) string {
	return argumentSummary("view image", arguments)
}
func (t *viewImageTool) Execute(ctx context.Context, arguments string) (Result, error) {
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
	file, path, err := t.workspace.openRead(input.Path)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Result{}, err
	}
	if !info.Mode().IsRegular() {
		return Result{}, errors.New("image path is not a regular file")
	}
	if info.Size() > attachment.MaxImageBytes {
		return Result{}, errors.New("image exceeds the 20 MiB limit")
	}
	memoryBytes := info.Size() + int64(base64.StdEncoding.EncodedLen(int(info.Size())))
	release, err := viewImageMemory.acquire(ctx, memoryBytes)
	if err != nil {
		return Result{}, err
	}
	defer release()
	data, err := io.ReadAll(io.LimitReader(file, info.Size()+1))
	if err != nil {
		return Result{}, err
	}
	if int64(len(data)) > info.Size() {
		return Result{}, errors.New("image changed size while it was being read")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("decode image: %w", err)
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/" + format
	}
	return Result{Content: fmt.Sprintf("Loaded %s (%s, %dx%d, %d bytes).", t.workspace.display(path), mime, config.Width, config.Height, len(data)), Images: []provider.Image{{MIMEType: mime, Data: base64.StdEncoding.EncodeToString(data), Detail: input.Detail}}}, nil
}
