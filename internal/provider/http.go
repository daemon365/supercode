package provider

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const MaxHTTPResponseBytes = int64(64 * 1024 * 1024)

var ErrHTTPResponseTooLarge = errors.New("provider response exceeds the 64 MiB limit")

// SecureHTTPClient bounds decompressed response bodies and rejects cross-origin
// redirects so API keys and custom authentication headers cannot be forwarded
// to a different endpoint. The returned client is an independent shallow copy.
func SecureHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	result := *base
	transport := result.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	result.Transport = boundedRoundTripper{next: transport, limit: MaxHTTPResponseBytes}
	previous := result.CheckRedirect
	result.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("provider redirect limit exceeded")
		}
		if len(via) > 0 && !sameOrigin(via[0].URL, request.URL) {
			return errors.New("provider cross-origin redirect is blocked")
		}
		if previous != nil {
			return previous(request, via)
		}
		return nil
	}
	return &result
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// SecureHTTPDoer adds the response-body boundary to SDK-specific clients. A
// concrete *http.Client should be passed through SecureHTTPClient as well so
// redirect policy can be enforced before the request is followed.
func SecureHTTPDoer(next HTTPDoer) HTTPDoer {
	return boundedDoer{next: next, limit: MaxHTTPResponseBytes}
}

func sameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

type boundedRoundTripper struct {
	next  http.RoundTripper
	limit int64
}

func (t boundedRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err == nil && response != nil && response.Body != nil {
		response.Body = NewBoundedResponseBody(response.Body, t.limit)
	}
	return response, err
}

type boundedDoer struct {
	next  HTTPDoer
	limit int64
}

func (d boundedDoer) Do(request *http.Request) (*http.Response, error) {
	response, err := d.next.Do(request)
	if err == nil && response != nil && response.Body != nil {
		response.Body = NewBoundedResponseBody(response.Body, d.limit)
	}
	return response, err
}

func NewBoundedResponseBody(body io.ReadCloser, limit int64) io.ReadCloser {
	return &boundedResponseBody{body: body, remaining: limit}
}

type boundedResponseBody struct {
	body      io.ReadCloser
	remaining int64
	exceeded  bool
}

func (b *boundedResponseBody) Read(value []byte) (int, error) {
	if b.exceeded {
		return 0, ErrHTTPResponseTooLarge
	}
	if b.remaining <= 0 {
		var probe [1]byte
		count, err := b.body.Read(probe[:])
		if count > 0 {
			b.exceeded = true
			return 0, ErrHTTPResponseTooLarge
		}
		return 0, err
	}
	if int64(len(value)) > b.remaining {
		value = value[:b.remaining]
	}
	count, err := b.body.Read(value)
	b.remaining -= int64(count)
	return count, err
}

func (b *boundedResponseBody) Close() error { return b.body.Close() }

type redactedWrappedError struct {
	prefix, message string
	cause           error
}

func (e redactedWrappedError) Error() string { return e.prefix + ": " + e.message }
func (e redactedWrappedError) Unwrap() error { return e.cause }

func RedactedError(prefix string, err error, secrets ...string) error {
	message := err.Error()
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return redactedWrappedError{prefix: prefix, message: message, cause: err}
}
