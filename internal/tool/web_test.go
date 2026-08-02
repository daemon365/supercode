package tool

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/daemon365/supercode/internal/permission"
)

func TestWebSearchDomainFilterDoesNotAuthorizeSearchEndpoint(t *testing.T) {
	permissions, err := permission.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := permissions.Grant(permission.Profile{Network: permission.Network{
		Domains: []string{"example.com"}, Protocols: []string{"https"},
	}}, permission.ScopeTurn); err != nil {
		t.Fatal(err)
	}
	web := newWebTool(permissions)
	arguments := `{"search_query":[{"q":"release notes","domains":["example.com"]}]}`
	if risk := web.Risk(arguments); risk != RiskNetwork {
		t.Fatalf("risk = %q, want network", risk)
	}
	request, err := web.PermissionRequest(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Permissions.Network.Domains) != 1 || request.Permissions.Network.Domains[0] != "html.duckduckgo.com" {
		t.Fatalf("requested domains = %v", request.Permissions.Network.Domains)
	}
	if _, err := permissions.Grant(request.Permissions, permission.ScopeTurn); err != nil {
		t.Fatal(err)
	}
	if risk := web.Risk(arguments); risk != RiskRead {
		t.Fatalf("risk after endpoint grant = %q, want read", risk)
	}
}

func TestWebFetchRejectsUnauthorizedRedirectHop(t *testing.T) {
	permissions, err := permission.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := permissions.Grant(permission.Profile{Network: permission.Network{
		Domains: []string{"127.0.0.1"}, Protocols: []string{"http"},
	}}, permission.ScopeTurn); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	var server *httptest.Server
	server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			target := strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/final"
			http.Redirect(writer, request, target, http.StatusFound)
			return
		}
		_, _ = writer.Write([]byte("done"))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()

	web := newWebTool(permissions)
	web.client = server.Client()
	web.client.CheckRedirect = web.checkRedirect
	if _, err := web.fetch(context.Background(), server.URL+"/redirect"); err == nil || !strings.Contains(err.Error(), "redirect endpoint is not authorized") {
		t.Fatalf("fetch redirect error = %v", err)
	}
	if _, err := permissions.Grant(permission.Profile{Network: permission.Network{
		Domains: []string{"localhost"}, Protocols: []string{"http"},
	}}, permission.ScopeTurn); err != nil {
		t.Fatal(err)
	}
	page, err := web.fetch(context.Background(), server.URL+"/redirect")
	if err != nil {
		t.Fatal(err)
	}
	if page.Body != "done" {
		t.Fatalf("redirected body = %q", page.Body)
	}
}

func TestWebCacheEvictsLeastRecentlyUsedPagesByBytes(t *testing.T) {
	web := newWebTool(nil)
	web.cacheLimit = 500
	first := web.store(webPage{URL: "https://one.example", Body: strings.Repeat("a", 100)})
	second := web.store(webPage{URL: "https://two.example", Body: strings.Repeat("b", 100)})
	if _, ok := web.lookup(first); !ok {
		t.Fatal("first page missing before eviction")
	}
	third := web.store(webPage{URL: "https://three.example", Body: strings.Repeat("c", 100)})
	if _, ok := web.lookup(first); !ok {
		t.Fatal("recently used page was evicted")
	}
	if _, ok := web.lookup(second); ok {
		t.Fatal("least recently used page was retained")
	}
	if _, ok := web.lookup(third); !ok {
		t.Fatal("new page was evicted")
	}
	if web.cacheBytes > web.cacheLimit {
		t.Fatalf("cache bytes = %d, limit = %d", web.cacheBytes, web.cacheLimit)
	}
}

func TestWebFindPreservesOriginalUnicodeOffsets(t *testing.T) {
	web := newWebTool(nil)
	ref := web.store(webPage{URL: "https://cached.example", Body: strings.Repeat("Ⱥ", 301) + " Needle " + strings.Repeat("界", 300)})
	value, err := web.find(ref, "needle")
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(map[string]any)
	if !ok || result["found"] != true {
		t.Fatalf("find result = %#v", value)
	}
	excerpt, _ := result["excerpt"].(string)
	if !strings.Contains(excerpt, "Needle") || !utf8.ValidString(excerpt) {
		t.Fatalf("invalid Unicode excerpt = %q", excerpt)
	}
}

func TestWebRiskKeepsLocalOperationsReadOnlyAndScreenshotExecutable(t *testing.T) {
	web := newWebTool(nil)
	if risk := web.Risk(`{"time":[{"utc_offset":"+08:00"}]}`); risk != RiskRead {
		t.Fatalf("time risk = %q", risk)
	}
	if request, err := web.PermissionRequest(`{"time":[{"utc_offset":"+08:00"}]}`); err != nil || !permission.Empty(request.Permissions) {
		t.Fatalf("time permission request = %+v, %v", request, err)
	}
	page := web.store(webPage{URL: "https://cached.example", Body: "cached"})
	if risk := web.Risk(`{"open":[{"ref_id":"` + page + `"}]}`); risk != RiskRead {
		t.Fatalf("cached open risk = %q", risk)
	}
	pdf := web.store(webPage{URL: "https://cached.example/file.pdf", Body: "%PDF-1.7", ContentType: "application/pdf"})
	arguments := `{"screenshot":[{"ref_id":"` + pdf + `","pageno":0}]}`
	if risk := web.Risk(arguments); risk != RiskExecute {
		t.Fatalf("screenshot risk = %q, want execute", risk)
	}
	if request, err := web.PermissionRequest(arguments); err != nil || !permission.Empty(request.Permissions) {
		t.Fatalf("screenshot permission request = %+v, %v", request, err)
	}
}

func TestWebRejectsOperationExplosionAndNegativeScreenshotPage(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			MaxItems int `json:"maxItems"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&webTool{}).Definition().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["search_query"].MaxItems != 4 || schema.Properties["screenshot"].MaxItems != maxWebResultImages {
		t.Fatalf("web schema array limits = %+v", schema.Properties)
	}

	var input webInput
	input.Search = make([]struct {
		Q       string   `json:"q"`
		Domains []string `json:"domains"`
		Recency int      `json:"recency"`
	}, 5)
	if err := validateWebInput(input); err == nil || !strings.Contains(err.Error(), "search_query") {
		t.Fatalf("search operation limit error = %v", err)
	}

	input = webInput{}
	input.Time = make([]struct {
		Offset string `json:"utc_offset"`
	}, 8)
	input.Finance = make([]struct {
		Ticker string `json:"ticker"`
		Type   string `json:"type"`
		Market string `json:"market"`
	}, 8)
	input.Open = make([]struct {
		RefID string `json:"ref_id"`
		Line  int    `json:"lineno"`
	}, 1)
	if err := validateWebInput(input); err == nil || !strings.Contains(err.Error(), "total limit") {
		t.Fatalf("total operation limit error = %v", err)
	}

	web := newWebTool(nil)
	pdf := web.store(webPage{URL: "https://cached.example/file.pdf", Body: "%PDF-1.7", ContentType: "application/pdf"})
	_, err := web.Execute(context.Background(), `{"screenshot":[{"ref_id":"`+pdf+`","pageno":-1}]}`)
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("negative screenshot page error = %v", err)
	}
	if _, err := decodeWebInput(strings.Repeat(" ", maxWebArgumentsBytes+1)); err == nil || !strings.Contains(err.Error(), "arguments exceed") {
		t.Fatalf("oversized web arguments error = %v", err)
	}
}

func TestPublicWebIPRejectsSpecialAndNonUnicastAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "224.0.0.1", "255.255.255.255", "100.64.0.1", "198.18.0.1", "::1", "ff02::1", "fe80::1", "fc00::1"} {
		if publicWebIP(net.ParseIP(raw)) {
			t.Errorf("publicWebIP(%s) = true", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicWebIP(net.ParseIP(raw)) {
			t.Errorf("publicWebIP(%s) = false", raw)
		}
	}
}

func TestWebScreenshotChecksAggregateByteLimitBeforeEncoding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake pdftoppm script is Unix-specific")
	}
	bin := t.TempDir()
	renderer := filepath.Join(bin, "pdftoppm")
	script := "#!/bin/sh\nfor value in \"$@\"; do prefix=\"$value\"; done\nprintf 'not-a-small-png-payload' > \"${prefix}.png\"\n"
	if err := os.WriteFile(renderer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	web := newWebTool(nil)
	ref := web.store(webPage{URL: "https://cached.example/file.pdf", Body: "%PDF-1.7", ContentType: "application/pdf"})
	_, image, _, err := web.screenshot(context.Background(), ref, 0, 10)
	if err == nil || !strings.Contains(err.Error(), "total limit") || image != nil {
		t.Fatalf("screenshot byte-limit result image=%v err=%v", image, err)
	}
}
