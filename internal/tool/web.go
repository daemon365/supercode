package tool

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/daemon365/supercode/internal/attachment"
	"github.com/daemon365/supercode/internal/permission"
	"github.com/daemon365/supercode/internal/provider"
)

type webTool struct {
	client      *http.Client
	mu          sync.Mutex
	next        int
	pages       map[string]*webCacheEntry
	recent      *list.List
	cacheBytes  int64
	cacheLimit  int64
	permissions *permission.Manager
}
type webPage struct {
	URL, Body, ContentType string
	Links                  []string
}

type webCacheEntry struct {
	id      string
	page    webPage
	bytes   int64
	element *list.Element
}

const (
	defaultWebCacheBytes = 16 * 1024 * 1024
	duckDuckGoEndpoint   = "https://html.duckduckgo.com/"
	bingImagesEndpoint   = "https://www.bing.com/"
	yahooFinanceEndpoint = "https://query1.finance.yahoo.com/"
	weatherGeoEndpoint   = "https://geocoding-api.open-meteo.com/"
	weatherAPIEndpoint   = "https://api.open-meteo.com/"
	espnSportsEndpoint   = "https://site.api.espn.com/"
	maxCachedPageLinks   = 256
	maxCachedLinkBytes   = 4 * 1024 * 1024
	maxWebURLBytes       = 16 * 1024
	maxWebArgumentsBytes = 1024 * 1024
	maxWebOperations     = 16
	maxWebResultImages   = 2
	maxWebResultImageRaw = 20 * 1024 * 1024
	maxWebScreenshotSide = 2048
	webScreenshotTimeout = 30 * time.Second
)

var (
	anchorPattern   = regexp.MustCompile(`(?is)<a[^>]+href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	tagPattern      = regexp.MustCompile(`(?is)<(?:script|style)[^>]*>.*?</(?:script|style)>|<[^>]+>`)
	spacePattern    = regexp.MustCompile(`[ \t\r\f\v]+`)
	imageURLPattern = regexp.MustCompile(`(?i)murl(?:&quot;|\")?\s*:\s*(?:&quot;|\")([^"&]+)`)
)

func newWebTool(permissions *permission.Manager) *webTool {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if !publicWebIP(address.IP) {
				return nil, errors.New("web access to private or local addresses is blocked")
			}
		}
		if len(addresses) == 0 {
			return nil, errors.New("host resolved to no addresses")
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}}
	result := &webTool{
		pages: make(map[string]*webCacheEntry), recent: list.New(), cacheLimit: defaultWebCacheBytes,
		permissions: permissions,
	}
	result.client = &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: result.checkRedirect}
	return result
}

func (*webTool) Category() Category { return CategoryNetwork }

func (*webTool) Definition() provider.ToolDefinition {
	return webDefinitionWithLimits(definition("web__run", "Run web operations. Supports search_query, open, click, find, screenshot (PDF pages), image_query, finance, weather, sports, and time. Network requests require approval.", `{"type":"object","properties":{"search_query":{"type":"array","items":{"type":"object","properties":{"q":{"type":"string"},"domains":{"type":"array","items":{"type":"string"}},"recency":{"type":"integer"}},"required":["q"]}},"open":{"type":"array","items":{"type":"object","properties":{"ref_id":{"type":"string"},"lineno":{"type":"integer"}},"required":["ref_id"]}},"click":{"type":"array","items":{"type":"object","properties":{"ref_id":{"type":"string"},"id":{"type":"integer"}},"required":["ref_id","id"]}},"find":{"type":"array","items":{"type":"object","properties":{"ref_id":{"type":"string"},"pattern":{"type":"string"}},"required":["ref_id","pattern"]}},"screenshot":{"type":"array","items":{"type":"object","properties":{"ref_id":{"type":"string"},"pageno":{"type":"integer","minimum":0}},"required":["ref_id","pageno"]}},"image_query":{"type":"array","items":{"type":"object","properties":{"q":{"type":"string"},"domains":{"type":"array","items":{"type":"string"}},"recency":{"type":"integer"}},"required":["q"]}},"finance":{"type":"array","items":{"type":"object","properties":{"ticker":{"type":"string"},"type":{"type":"string"},"market":{"type":"string"}},"required":["ticker","type"]}},"weather":{"type":"array","items":{"type":"object","properties":{"location":{"type":"string"},"start":{"type":"string"},"duration":{"type":"integer"}},"required":["location"]}},"sports":{"type":"array","items":{"type":"object","properties":{"fn":{"type":"string","enum":["schedule","standings"]},"league":{"type":"string"},"team":{"type":"string"},"date_from":{"type":"string"},"date_to":{"type":"string"},"num_games":{"type":"integer"}},"required":["fn","league"]}},"time":{"type":"array","items":{"type":"object","properties":{"utc_offset":{"type":"string"}},"required":["utc_offset"]}},"response_length":{"type":"string","enum":["short","medium","long"]}},"additionalProperties":false}`))
}

func webDefinitionWithLimits(result provider.ToolDefinition) provider.ToolDefinition {
	var schema map[string]any
	if json.Unmarshal(result.Parameters, &schema) != nil {
		return result
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return result
	}
	limits := map[string]int{
		"search_query": 4, "image_query": 4, "open": 8, "click": 8, "find": 8,
		"screenshot": maxWebResultImages, "finance": 8, "weather": 8, "sports": 8, "time": 8,
	}
	for name, limit := range limits {
		if property, exists := properties[name].(map[string]any); exists {
			property["maxItems"] = limit
		}
	}
	for _, name := range []string{"search_query", "image_query"} {
		property, _ := properties[name].(map[string]any)
		items, _ := property["items"].(map[string]any)
		itemProperties, _ := items["properties"].(map[string]any)
		domains, _ := itemProperties["domains"].(map[string]any)
		if domains != nil {
			domains["maxItems"] = 20
		}
	}
	if encoded, err := json.Marshal(schema); err == nil {
		result.Parameters = encoded
	}
	return result
}
func (t *webTool) Risk(arguments string) Risk {
	endpoints, err := t.requiredNetworkEndpoints(arguments)
	if err != nil {
		return RiskNetwork
	}
	var input webInput
	if json.Unmarshal([]byte(arguments), &input) != nil {
		return RiskNetwork
	}
	if len(endpoints) == 0 {
		if len(input.Screenshot) > 0 {
			return RiskExecute
		}
		return RiskRead
	}
	allowed := t.endpointsAllowed(endpoints)
	if allowed && len(input.Screenshot) == 0 {
		return RiskRead
	}
	if allowed {
		return RiskExecute
	}
	return RiskNetwork
}

func (t *webTool) PermissionRequest(arguments string) (permission.Request, error) {
	endpoints, err := t.requiredNetworkEndpoints(arguments)
	if err != nil {
		return permission.Request{}, err
	}
	if len(endpoints) == 0 {
		return permission.Request{}, nil
	}
	profile := permission.Profile{}
	seenDomains := make(map[string]struct{})
	seenProtocols := make(map[string]struct{})
	for _, endpoint := range endpoints {
		parsed, parseErr := url.Parse(endpoint)
		if parseErr != nil || parsed.Hostname() == "" || !slicesContains([]string{"http", "https"}, parsed.Scheme) {
			return permission.Request{}, fmt.Errorf("invalid web endpoint %q", endpoint)
		}
		if _, exists := seenDomains[parsed.Hostname()]; !exists {
			profile.Network.Domains = append(profile.Network.Domains, parsed.Hostname())
			seenDomains[parsed.Hostname()] = struct{}{}
		}
		if _, exists := seenProtocols[parsed.Scheme]; !exists {
			profile.Network.Protocols = append(profile.Network.Protocols, parsed.Scheme)
			seenProtocols[parsed.Scheme] = struct{}{}
		}
	}
	sort.Strings(profile.Network.Domains)
	sort.Strings(profile.Network.Protocols)
	return permission.Request{Reason: "access the requested web endpoints", Permissions: profile}, nil
}

func (t *webTool) requiredNetworkEndpoints(arguments string) ([]string, error) {
	input, err := decodeWebInput(arguments)
	if err != nil {
		return nil, err
	}
	var endpoints []string
	for range input.Search {
		endpoints = append(endpoints, duckDuckGoEndpoint)
	}
	for range input.Images {
		endpoints = append(endpoints, bingImagesEndpoint)
	}
	for _, request := range input.Open {
		target := request.RefID
		if page, ok := t.lookup(target); ok {
			if page.Body != "" {
				continue
			}
			target = page.URL
		}
		endpoints = append(endpoints, target)
	}
	for _, request := range input.Click {
		page, ok := t.lookup(request.RefID)
		if !ok {
			return nil, fmt.Errorf("unknown ref_id %q", request.RefID)
		}
		if request.ID < 0 || request.ID >= len(page.Links) {
			return nil, errors.New("link id is out of range")
		}
		endpoints = append(endpoints, page.Links[request.ID])
	}
	for range input.Finance {
		endpoints = append(endpoints, yahooFinanceEndpoint)
	}
	for range input.WeatherRaw {
		endpoints = append(endpoints, weatherGeoEndpoint, weatherAPIEndpoint)
	}
	for range input.Sports {
		endpoints = append(endpoints, espnSportsEndpoint)
	}
	endpoints = uniqueStrings(endpoints)
	for _, endpoint := range endpoints {
		if len(endpoint) > maxWebURLBytes {
			return nil, fmt.Errorf("web URL exceeds the %d KiB limit", maxWebURLBytes/1024)
		}
	}
	return endpoints, nil
}

func (t *webTool) endpointsAllowed(endpoints []string) bool {
	if t.permissions == nil {
		return false
	}
	for _, endpoint := range endpoints {
		if !t.permissions.AllowsURL(endpoint) {
			return false
		}
	}
	return true
}
func (*webTool) Summary(arguments string) string { return argumentSummary("access web", arguments) }

type webInput struct {
	Search []struct {
		Q       string   `json:"q"`
		Domains []string `json:"domains"`
		Recency int      `json:"recency"`
	} `json:"search_query"`
	Open []struct {
		RefID string `json:"ref_id"`
		Line  int    `json:"lineno"`
	} `json:"open"`
	Click []struct {
		RefID string `json:"ref_id"`
		ID    int    `json:"id"`
	} `json:"click"`
	Find    []struct{ RefID, Pattern string } `json:"-"`
	FindRaw []struct {
		RefID   string `json:"ref_id"`
		Pattern string `json:"pattern"`
	} `json:"find"`
	Screenshot []struct {
		RefID string `json:"ref_id"`
		Page  int    `json:"pageno"`
	} `json:"screenshot"`
	Images []struct {
		Q       string   `json:"q"`
		Domains []string `json:"domains"`
		Recency int      `json:"recency"`
	} `json:"image_query"`
	Finance []struct {
		Ticker string `json:"ticker"`
		Type   string `json:"type"`
		Market string `json:"market"`
	} `json:"finance"`
	Weather []struct {
		Location, Start string
		Duration        int
	} `json:"-"`
	WeatherRaw []struct {
		Location string `json:"location"`
		Start    string `json:"start"`
		Duration int    `json:"duration"`
	} `json:"weather"`
	Sports []struct {
		Fn       string `json:"fn"`
		League   string `json:"league"`
		Team     string `json:"team"`
		DateFrom string `json:"date_from"`
		DateTo   string `json:"date_to"`
		Num      int    `json:"num_games"`
	} `json:"sports"`
	Time []struct {
		Offset string `json:"utc_offset"`
	} `json:"time"`
	Length string `json:"response_length"`
}

func decodeWebInput(arguments string) (webInput, error) {
	var input webInput
	if len(arguments) > maxWebArgumentsBytes {
		return input, fmt.Errorf("web arguments exceed the %d MiB limit", maxWebArgumentsBytes/(1024*1024))
	}
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return input, fmt.Errorf("invalid web arguments: %w", err)
	}
	if err := validateWebInput(input); err != nil {
		return input, err
	}
	return input, nil
}

func validateWebInput(input webInput) error {
	limits := []struct {
		name  string
		count int
		limit int
	}{
		{"search_query", len(input.Search), 4},
		{"image_query", len(input.Images), 4},
		{"open", len(input.Open), 8},
		{"click", len(input.Click), 8},
		{"find", len(input.FindRaw), 8},
		{"screenshot", len(input.Screenshot), maxWebResultImages},
		{"finance", len(input.Finance), 8},
		{"weather", len(input.WeatherRaw), 8},
		{"sports", len(input.Sports), 8},
		{"time", len(input.Time), 8},
	}
	total := 0
	for _, item := range limits {
		if item.count > item.limit {
			return fmt.Errorf("web %s supports at most %d operations per call", item.name, item.limit)
		}
		total += item.count
	}
	if total > maxWebOperations {
		return fmt.Errorf("web call exceeds the %d-operation total limit", maxWebOperations)
	}
	for _, query := range input.Search {
		if len(query.Domains) > 20 {
			return errors.New("web search_query supports at most 20 domain filters")
		}
	}
	for _, query := range input.Images {
		if len(query.Domains) > 20 {
			return errors.New("web image_query supports at most 20 domain filters")
		}
	}
	for _, request := range input.Screenshot {
		if request.Page < 0 {
			return errors.New("web screenshot pageno must be non-negative")
		}
	}
	return nil
}

func publicWebIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsMulticast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		// RFC 6598 shared address space and the benchmarking range are not
		// publicly routable, but Go intentionally classifies them as global
		// unicast rather than private.
		if ipv4[0] == 100 && ipv4[1]&0xc0 == 0x40 {
			return false
		}
		if ipv4[0] == 198 && (ipv4[1] == 18 || ipv4[1] == 19) {
			return false
		}
	}
	return true
}

func (t *webTool) Execute(ctx context.Context, arguments string) (Result, error) {
	input, err := decodeWebInput(arguments)
	if err != nil {
		return Result{}, err
	}
	var results []any
	var images []provider.Image
	var imageBytes int64
	for _, query := range input.Search {
		value, err := t.search(ctx, query.Q, query.Domains, query.Recency, false)
		results = append(results, outcome("search_query", value, err))
	}
	for _, query := range input.Images {
		value, err := t.search(ctx, query.Q, query.Domains, query.Recency, true)
		results = append(results, outcome("image_query", value, err))
	}
	for _, request := range input.Open {
		value, err := t.open(ctx, request.RefID, request.Line)
		results = append(results, outcome("open", value, err))
	}
	for _, request := range input.Click {
		value, err := t.click(ctx, request.RefID, request.ID)
		results = append(results, outcome("click", value, err))
	}
	for _, request := range input.FindRaw {
		value, err := t.find(request.RefID, request.Pattern)
		results = append(results, outcome("find", value, err))
	}
	for _, request := range input.Screenshot {
		value, image, rawBytes, err := t.screenshot(ctx, request.RefID, request.Page, maxWebResultImageRaw-imageBytes)
		if image != nil {
			if len(images) >= maxWebResultImages || rawBytes > maxWebResultImageRaw-imageBytes {
				err = fmt.Errorf("web result images exceed the %d MiB total limit", maxWebResultImageRaw/(1024*1024))
			} else {
				images = append(images, *image)
				imageBytes += rawBytes
			}
		}
		results = append(results, outcome("screenshot", value, err))
	}
	for _, request := range input.Finance {
		value, err := t.finance(ctx, request.Ticker, request.Type, request.Market)
		results = append(results, outcome("finance", value, err))
	}
	for _, request := range input.WeatherRaw {
		value, err := t.weather(ctx, request.Location, request.Start, request.Duration)
		results = append(results, outcome("weather", value, err))
	}
	for _, request := range input.Sports {
		value, err := t.sports(ctx, request.Fn, request.League, request.Team, request.DateFrom, request.DateTo, request.Num)
		results = append(results, outcome("sports", value, err))
	}
	for _, request := range input.Time {
		value, err := timeAtOffset(request.Offset)
		results = append(results, outcome("time", value, err))
	}
	if len(results) == 0 {
		return Result{}, errors.New("at least one web operation is required")
	}
	encoded, _ := json.MarshalIndent(results, "", "  ")
	return Result{Content: truncateWebResponse(string(encoded), input.Length), Images: images}, nil
}
func outcome(kind string, value any, err error) map[string]any {
	result := map[string]any{"type": kind}
	if err != nil {
		result["error"] = err.Error()
	} else {
		result["result"] = value
	}
	return result
}

func (t *webTool) search(ctx context.Context, query string, domains []string, recency int, imageSearch bool) (any, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query is required")
	}
	for _, domain := range domains {
		query += " site:" + domain
	}
	if recency > 0 {
		query += " after:" + time.Now().UTC().AddDate(0, 0, -recency).Format("2006-01-02")
	}
	endpoint := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	if imageSearch {
		endpoint = "https://www.bing.com/images/search?q=" + url.QueryEscape(query)
	}
	page, err := t.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if imageSearch {
		matches := imageURLPattern.FindAllStringSubmatch(page.Body, 10)
		items := []any{}
		for _, match := range matches {
			target := html.UnescapeString(match[1])
			if len(target) > maxWebURLBytes {
				continue
			}
			ref := t.store(webPage{URL: target})
			items = append(items, map[string]any{"ref_id": ref, "image_url": target})
		}
		if len(items) == 0 {
			return nil, errors.New("image search returned no parseable results")
		}
		return items, nil
	}
	items := []any{}
	for _, match := range anchorPattern.FindAllStringSubmatch(page.Body, maxCachedPageLinks) {
		target := html.UnescapeString(match[1])
		title := strings.TrimSpace(plainText(match[2]))
		if title == "" || strings.HasPrefix(target, "/") {
			continue
		}
		if parsed, err := url.Parse(target); err == nil {
			if redirected := parsed.Query().Get("uddg"); redirected != "" {
				target = redirected
			}
		}
		if len(target) > maxWebURLBytes {
			continue
		}
		ref := t.store(webPage{URL: target})
		items = append(items, map[string]any{"ref_id": ref, "title": title, "url": target})
		if len(items) >= 10 {
			break
		}
	}
	if len(items) == 0 {
		return nil, errors.New("search returned no parseable results")
	}
	return items, nil
}
func (t *webTool) open(ctx context.Context, reference string, line int) (any, error) {
	page, ok := t.lookup(reference)
	if !ok {
		page = webPage{URL: reference}
	}
	if page.Body == "" {
		fetched, err := t.fetch(ctx, page.URL)
		if err != nil {
			return nil, err
		}
		page = fetched
		reference = t.store(page)
	}
	text := plainText(page.Body)
	lines := strings.Split(text, "\n")
	if line > 0 && line <= len(lines) {
		start := line - 1
		end := min(len(lines), start+80)
		text = strings.Join(lines[start:end], "\n")
	}
	return map[string]any{"ref_id": reference, "url": page.URL, "content_type": page.ContentType, "text": truncate(text), "links": numberedLinks(page.Links)}, nil
}
func (t *webTool) click(ctx context.Context, reference string, id int) (any, error) {
	page, ok := t.lookup(reference)
	if !ok {
		return nil, errors.New("unknown ref_id")
	}
	if id < 0 || id >= len(page.Links) {
		return nil, errors.New("link id is out of range")
	}
	return t.open(ctx, page.Links[id], 0)
}
func (t *webTool) find(reference, pattern string) (any, error) {
	page, ok := t.lookup(reference)
	if !ok {
		return nil, errors.New("unknown ref_id")
	}
	text := plainText(page.Body)
	matcher, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(pattern))
	if err != nil {
		return nil, fmt.Errorf("compile find pattern: %w", err)
	}
	match := matcher.FindStringIndex(text)
	if match == nil {
		return map[string]any{"found": false}, nil
	}
	start := max(0, match[0]-300)
	end := min(len(text), match[1]+500)
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return map[string]any{"found": true, "excerpt": text[start:end]}, nil
}

func (t *webTool) fetch(ctx context.Context, target string) (webPage, error) {
	if len(target) > maxWebURLBytes {
		return webPage{}, fmt.Errorf("web URL exceeds the %d KiB limit", maxWebURLBytes/1024)
	}
	parsed, err := url.Parse(target)
	if err != nil || !slicesContains([]string{"http", "https"}, parsed.Scheme) {
		return webPage{}, errors.New("only http and https URLs are supported")
	}
	if t.permissions == nil || !t.permissions.AllowsURL(target) {
		return webPage{}, fmt.Errorf("web endpoint is not authorized: %s://%s", parsed.Scheme, parsed.Hostname())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return webPage{}, err
	}
	request.Header.Set("User-Agent", "SuperCode/1.0")
	response, err := t.client.Do(request)
	if err != nil {
		return webPage{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return webPage{}, fmt.Errorf("HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil {
		return webPage{}, err
	}
	if len(data) > 4*1024*1024 {
		return webPage{}, errors.New("web response exceeds 4 MiB")
	}
	page := webPage{URL: response.Request.URL.String(), Body: string(data), ContentType: response.Header.Get("Content-Type")}
	linkBytes := 0
	for _, match := range anchorPattern.FindAllStringSubmatch(page.Body, maxCachedPageLinks) {
		link := html.UnescapeString(match[1])
		resolved, err := response.Request.URL.Parse(link)
		if err == nil {
			value := resolved.String()
			if len(value) > maxWebURLBytes || linkBytes+len(value) > maxCachedLinkBytes {
				continue
			}
			linkBytes += len(value)
			page.Links = append(page.Links, value)
		}
	}
	return page, nil
}

func (t *webTool) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(request.URL.String()) > maxWebURLBytes {
		return fmt.Errorf("redirect URL exceeds the %d KiB limit", maxWebURLBytes/1024)
	}
	if t.permissions == nil || !t.permissions.AllowsURL(request.URL.String()) {
		return fmt.Errorf("redirect endpoint is not authorized: %s://%s", request.URL.Scheme, request.URL.Hostname())
	}
	return nil
}
func (t *webTool) store(page webPage) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	id := "web" + strconv.Itoa(t.next)
	entry := &webCacheEntry{id: id, page: page, bytes: webPageBytes(page)}
	entry.element = t.recent.PushFront(entry)
	t.pages[id] = entry
	t.cacheBytes += entry.bytes
	for t.cacheBytes > t.cacheLimit && t.recent.Len() > 1 {
		oldest := t.recent.Back()
		candidate := oldest.Value.(*webCacheEntry)
		delete(t.pages, candidate.id)
		t.recent.Remove(oldest)
		t.cacheBytes -= candidate.bytes
	}
	return id
}
func (t *webTool) lookup(id string) (webPage, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.pages[id]
	if !ok {
		return webPage{}, false
	}
	t.recent.MoveToFront(entry.element)
	return entry.page, true
}

func webPageBytes(page webPage) int64 {
	// Include a conservative fixed charge for map/list/string headers in
	// addition to payload bytes so many URL-only search references stay bounded.
	result := int64(128 + len(page.URL) + len(page.Body) + len(page.ContentType))
	for _, link := range page.Links {
		result += int64(16 + len(link))
	}
	return result
}

func (t *webTool) finance(ctx context.Context, ticker, kind, market string) (any, error) {
	endpoint := "https://query1.finance.yahoo.com/v8/finance/chart/" + url.PathEscape(ticker) + "?range=5d&interval=1d"
	page, err := t.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal([]byte(page.Body), &value); err != nil {
		return nil, err
	}
	return map[string]any{"ticker": ticker, "type": kind, "market": market, "data": value}, nil
}
func (t *webTool) weather(ctx context.Context, location, start string, duration int) (any, error) {
	geo, err := t.fetch(ctx, "https://geocoding-api.open-meteo.com/v1/search?count=1&name="+url.QueryEscape(location))
	if err != nil {
		return nil, err
	}
	var found struct {
		Results []struct {
			Latitude, Longitude float64
			Name, Country       string
		}
	}
	if err := json.Unmarshal([]byte(geo.Body), &found); err != nil || len(found.Results) == 0 {
		return nil, errors.New("weather location not found")
	}
	point := found.Results[0]
	if duration <= 0 {
		duration = 7
	}
	if duration > 16 {
		duration = 16
	}
	endpoint := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%f&longitude=%f&current=temperature_2m,weather_code&daily=weather_code,temperature_2m_max,temperature_2m_min", point.Latitude, point.Longitude)
	if start != "" {
		startDate, err := time.Parse("2006-01-02", start)
		if err != nil {
			return nil, errors.New("weather start must use YYYY-MM-DD")
		}
		end := startDate.AddDate(0, 0, duration-1).Format("2006-01-02")
		endpoint += "&start_date=" + url.QueryEscape(start) + "&end_date=" + url.QueryEscape(end)
	} else {
		endpoint += "&forecast_days=" + strconv.Itoa(duration)
	}
	page, err := t.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var data any
	if err := json.Unmarshal([]byte(page.Body), &data); err != nil {
		return nil, err
	}
	return map[string]any{"location": point.Name + ", " + point.Country, "data": data}, nil
}
func (t *webTool) sports(ctx context.Context, function, league, team, from, to string, count int) (any, error) {
	paths := map[string]string{"nba": "basketball/nba", "wnba": "basketball/wnba", "nfl": "football/nfl", "nhl": "hockey/nhl", "mlb": "baseball/mlb", "epl": "soccer/eng.1", "ncaamb": "basketball/mens-college-basketball", "ncaawb": "basketball/womens-college-basketball", "ipl": "cricket/8048"}
	path := paths[strings.ToLower(league)]
	if path == "" {
		return nil, errors.New("unsupported sports league")
	}
	suffix := "scoreboard"
	if function == "standings" {
		suffix = "standings"
	}
	endpoint := "https://site.api.espn.com/apis/site/v2/sports/" + path + "/" + suffix
	values := url.Values{}
	if team != "" {
		values.Set("teams", team)
	}
	if from != "" {
		dateRange := strings.ReplaceAll(from, "-", "")
		if to != "" {
			dateRange += "-" + strings.ReplaceAll(to, "-", "")
		}
		values.Set("dates", dateRange)
	}
	if count > 0 {
		values.Set("limit", strconv.Itoa(count))
	}
	if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	page, err := t.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var data any
	if err := json.Unmarshal([]byte(page.Body), &data); err != nil {
		return nil, err
	}
	return data, nil
}
func timeAtOffset(offset string) (any, error) {
	sign := 1
	if strings.HasPrefix(offset, "-") {
		sign = -1
	} else if !strings.HasPrefix(offset, "+") {
		return nil, errors.New("utc_offset must look like +08:00")
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(offset, "+"), "-"), ":")
	if len(parts) != 2 {
		return nil, errors.New("utc_offset must look like +08:00")
	}
	hours, e1 := strconv.Atoi(parts[0])
	minutes, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil || hours > 14 || minutes > 59 {
		return nil, errors.New("invalid UTC offset")
	}
	seconds := sign * (hours*3600 + minutes*60)
	now := time.Now().In(time.FixedZone(offset, seconds))
	return map[string]any{"utc_offset": offset, "datetime": now.Format(time.RFC3339)}, nil
}

func (t *webTool) screenshot(ctx context.Context, reference string, pageNumber int, maximumBytes int64) (any, *provider.Image, int64, error) {
	if pageNumber < 0 {
		return nil, nil, 0, errors.New("screenshot pageno must be non-negative")
	}
	page, ok := t.lookup(reference)
	if !ok {
		return nil, nil, 0, errors.New("unknown ref_id")
	}
	if !strings.Contains(strings.ToLower(page.ContentType), "pdf") && !strings.HasPrefix(page.Body, "%PDF") {
		return nil, nil, 0, errors.New("screenshot currently supports PDF references")
	}
	directory, err := os.MkdirTemp("", "supercode-pdf-*")
	if err != nil {
		return nil, nil, 0, err
	}
	defer os.RemoveAll(directory)
	input := directory + "/input.pdf"
	if err := os.WriteFile(input, []byte(page.Body), 0o600); err != nil {
		return nil, nil, 0, err
	}
	prefix := directory + "/page"
	renderContext, cancel := context.WithTimeout(ctx, webScreenshotTimeout)
	defer cancel()
	command := exec.CommandContext(renderContext, "pdftoppm", "-f", strconv.Itoa(pageNumber+1), "-l", strconv.Itoa(pageNumber+1), "-singlefile", "-scale-to", strconv.Itoa(maxWebScreenshotSide), "-png", input, prefix)
	configureProcessTree(command, false)
	command.WaitDelay = time.Second
	defer cleanupProcessTree(command)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		if errors.Is(renderContext.Err(), context.DeadlineExceeded) {
			return nil, nil, 0, fmt.Errorf("render PDF page timed out after %s", webScreenshotTimeout)
		}
		return nil, nil, 0, fmt.Errorf("render PDF page: %w: %s", err, output.String())
	}
	path := prefix + ".png"
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, 0, err
	}
	if maximumBytes < 0 || info.Size() > maximumBytes {
		return nil, nil, 0, fmt.Errorf("web result images exceed the %d MiB total limit", maxWebResultImageRaw/(1024*1024))
	}
	image, err := attachment.Load(path, "high")
	if err != nil {
		return nil, nil, 0, err
	}
	return map[string]any{"ref_id": reference, "pageno": pageNumber, "bytes": info.Size()}, &image, info.Size(), nil
}
func plainText(value string) string {
	value = tagPattern.ReplaceAllString(value, "\n")
	value = html.UnescapeString(value)
	lines := strings.Split(value, "\n")
	output := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(spacePattern.ReplaceAllString(line, " "))
		if line != "" {
			output = append(output, line)
		}
	}
	return strings.Join(output, "\n")
}
func numberedLinks(links []string) []map[string]any {
	if len(links) > 50 {
		links = links[:50]
	}
	result := make([]map[string]any, 0, len(links))
	for id, link := range links {
		result = append(result, map[string]any{"id": id, "url": link})
	}
	return result
}
func slicesContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func truncateWebResponse(value, length string) string {
	limit := 24 * 1024
	switch length {
	case "short":
		limit = 8 * 1024
	case "long":
		limit = maxToolOutput
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "\n[output truncated]"
}
