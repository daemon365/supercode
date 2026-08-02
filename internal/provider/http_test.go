package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecureHTTPClientRejectsCrossOriginRedirectWithSecretHeaders(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	leaked := false
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		leaked = request.Header.Get("X-Api-Key") != ""
	}))
	target.Listener = listener
	target.Start()
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer source.Close()
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, source.URL, nil)
	request.Header.Set("X-Api-Key", "secret")
	_, err = SecureHTTPClient(source.Client()).Do(request)
	if err == nil || !strings.Contains(err.Error(), "cross-origin") || leaked {
		t.Fatalf("redirect err=%v leaked=%t", err, leaked)
	}
}

func TestBoundedResponseBodyAndRedactedError(t *testing.T) {
	body := NewBoundedResponseBody(io.NopCloser(strings.NewReader("123456")), 5)
	value, err := io.ReadAll(body)
	if !errors.Is(err, ErrHTTPResponseTooLarge) || string(value) != "12345" {
		t.Fatalf("body=%q err=%v", value, err)
	}
	cause := context.Canceled
	wrapped := RedactedError("provider", fmt.Errorf("secret: %w", cause), "secret")
	if !strings.Contains(wrapped.Error(), "[REDACTED]") || !errors.Is(wrapped, cause) {
		t.Fatalf("error was not redacted: %v", wrapped)
	}
}
