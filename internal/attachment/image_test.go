package attachment

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFromBytesLoadsPNG(t *testing.T) {
	content, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nWQAAAAASUVORK5CYII=")
	image, err := FromBytes(content, "high")
	if err != nil {
		t.Fatal(err)
	}
	if image.MIMEType != "image/png" || image.Data == "" {
		t.Fatalf("image = %#v", image)
	}
}

func TestLoadRejectsOversizedFileBeforeReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxImageBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := Load(path, "high"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestBoundedImageOutputCapsSourceBytes(t *testing.T) {
	var output boundedImageOutput
	value := make([]byte, MaxImageBytes+1024)
	if count, err := output.Write(value); err != nil || count != len(value) {
		t.Fatalf("Write() = %d, %v", count, err)
	}
	if len(output.Bytes()) != MaxImageBytes || !output.exceeded {
		t.Fatalf("bounded output len=%d exceeded=%t", len(output.Bytes()), output.exceeded)
	}
}

func TestLoadClipboardTimesOutAndKillsHangingHelperTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake clipboard helper script is Unix-specific")
	}
	bin := t.TempDir()
	name := "wl-paste"
	if runtime.GOOS == "darwin" {
		name = "pngpaste"
	}
	helper := filepath.Join(bin, name)
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	started := time.Now()
	_, err := loadClipboardWithTimeout("high", 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("clipboard timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("clipboard helper tree was not terminated promptly: %s", elapsed)
	}
}

func TestLoadClipboardDoesNotWaitForeverForEscapedPipeHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake clipboard helper script is Unix-specific")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is unavailable")
	}
	bin := t.TempDir()
	name := "wl-paste"
	if runtime.GOOS == "darwin" {
		name = "pngpaste"
	}
	helper := filepath.Join(bin, name)
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nsetsid sh -c 'sleep 3' &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	started := time.Now()
	_, err := loadClipboardWithTimeout("high", 30*time.Millisecond)
	if err == nil {
		t.Fatal("escaped pipe holder unexpectedly returned an image")
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("clipboard helper waited for escaped pipe holder: %s", elapsed)
	}
}
