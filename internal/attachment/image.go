package attachment

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/daemon365/supercode/internal/provider"
)

const MaxImageBytes = 20 * 1024 * 1024

const clipboardCommandTimeout = 5 * time.Second

func Load(path, detail string) (provider.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return provider.Image{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return provider.Image{}, err
	}
	if !info.Mode().IsRegular() {
		return provider.Image{}, errors.New("image path is not a regular file")
	}
	if info.Size() > MaxImageBytes {
		return provider.Image{}, imageTooLargeError()
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxImageBytes+1))
	if err != nil {
		return provider.Image{}, err
	}
	return FromBytes(content, detail)
}

func FromBytes(content []byte, detail string) (provider.Image, error) {
	if len(content) == 0 {
		return provider.Image{}, errors.New("image is empty")
	}
	if len(content) > MaxImageBytes {
		return provider.Image{}, imageTooLargeError()
	}
	mime := http.DetectContentType(content)
	if !strings.HasPrefix(mime, "image/") {
		return provider.Image{}, errors.New("file is not a recognized image")
	}
	if detail == "" {
		detail = "high"
	}
	if detail != "high" && detail != "low" && detail != "original" {
		return provider.Image{}, errors.New("image detail must be low, high, or original")
	}
	return provider.Image{MIMEType: mime, Data: base64.StdEncoding.EncodeToString(content), Detail: detail}, nil
}

func LoadClipboard(detail string) (provider.Image, error) {
	return loadClipboardWithTimeout(detail, clipboardCommandTimeout)
}

func loadClipboardWithTimeout(detail string, timeout time.Duration) (provider.Image, error) {
	candidates := [][]string{{"wl-paste", "--no-newline", "--type", "image/png"}}
	if runtime.GOOS == "darwin" {
		candidates = [][]string{{"pngpaste", "-"}}
	}
	var failures []error
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate[0])
		if err != nil {
			continue
		}
		commandContext, cancel := context.WithTimeout(context.Background(), timeout)
		command := exec.CommandContext(commandContext, path, candidate[1:]...)
		configureClipboardProcess(command)
		command.WaitDelay = time.Second
		content := boundedImageOutput{onExceeded: func() {
			_ = terminateClipboardProcess(command)
		}}
		command.Stdout = &content
		err = command.Run()
		timedOut := errors.Is(commandContext.Err(), context.DeadlineExceeded)
		cancel()
		if content.exceeded {
			return provider.Image{}, imageTooLargeError()
		}
		if timedOut {
			failures = append(failures, fmt.Errorf("image clipboard helper timed out after %s", timeout))
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		return FromBytes(content.Bytes(), detail)
	}
	if len(failures) > 0 {
		return provider.Image{}, errors.Join(failures...)
	}
	return provider.Image{}, errors.New("image clipboard helper is unavailable (install wl-clipboard or pngpaste)")
}

func imageTooLargeError() error {
	return fmt.Errorf("image exceeds the %d MiB limit", MaxImageBytes/(1024*1024))
}

type boundedImageOutput struct {
	content    []byte
	exceeded   bool
	onExceeded func()
	exceedOnce sync.Once
}

func (b *boundedImageOutput) Write(value []byte) (int, error) {
	original := len(value)
	remaining := MaxImageBytes - len(b.content)
	if remaining > 0 {
		b.content = append(b.content, value[:min(len(value), remaining)]...)
	}
	if original > remaining {
		b.exceeded = true
		if b.onExceeded != nil {
			b.exceedOnce.Do(b.onExceeded)
		}
	}
	return original, nil
}

func (b *boundedImageOutput) Bytes() []byte { return b.content }
