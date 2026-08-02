package attachment

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/daemon365/supercode/internal/provider"
)

const MaxImageBytes = 20 * 1024 * 1024

func Load(path, detail string) (provider.Image, error) {
	content, err := os.ReadFile(path)
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
		return provider.Image{}, fmt.Errorf("image exceeds the %d MiB limit", MaxImageBytes/(1024*1024))
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
		content, err := exec.Command(path, candidate[1:]...).Output()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		return FromBytes(content, detail)
	}
	if len(failures) > 0 {
		return provider.Image{}, errors.Join(failures...)
	}
	return provider.Image{}, errors.New("image clipboard helper is unavailable (install wl-clipboard or pngpaste)")
}
