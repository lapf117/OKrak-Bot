package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// netClientContext builds an HTTP client honouring the configured proxy.
func netClient() *http.Client { return netClientContext(context.Background()) }

func netClientContext(ctx context.Context) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy := strings.TrimSpace(executionConfig(ctx).Proxy); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: 45 * time.Second, Transport: transport}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（直连）"
	}
	if u, err := url.Parse(s); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return s
}

func toolNetCheckContext(ctx context.Context, args map[string]interface{}) (string, error) {
	targets := executionConfig(ctx).NetCheckURLs
	if len(targets) == 0 {
		targets = []string{"https://api.telegram.org", "https://github.com", "https://pypi.org", "https://www.google.com", "https://www.baidu.com"}
	}
	var b strings.Builder
	b.WriteString("网络自检\n")
	fmt.Fprintf(&b, "代理: %s\n\n", orNone(executionConfig(ctx).Proxy))
	cl := netClientContext(ctx)
	defer cl.CloseIdleConnections()
	for _, t := range targets {
		start := time.Now()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, t, nil)
		if reqErr != nil {
			return b.String(), reqErr
		}
		resp, err := cl.Do(req)
		if err != nil {
			fmt.Fprintf(&b, "失败  %-32s %s\n", t, short(err.Error(), 60))
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		fmt.Fprintf(&b, "%d   %-32s %dms\n", resp.StatusCode, t, time.Since(start).Milliseconds())
	}
	if out, err := runShellContext(ctx, "getent hosts github.com | head -1", 10*time.Second); err == nil {
		b.WriteString("\nDNS: " + strings.TrimSpace(out))
	}
	return b.String(), nil
}

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg)[^>]*>.*?</(script|style|noscript|svg)>`)
	blockTagRe    = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr|/section|/article|/header|/footer|hr)[^>]*>`)
	htmlRe        = regexp.MustCompile(`<[^>]+>`)
	titleRe       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	blankLinesRe  = regexp.MustCompile(`\n{3,}`)
	spacesRe      = regexp.MustCompile(`[ \t\x{00a0}]+`)
)

// htmlToText keeps paragraph structure while dropping markup and scripts.
func htmlToText(page string) string {
	page = scriptStyleRe.ReplaceAllString(page, " ")
	page = blockTagRe.ReplaceAllString(page, "\n")
	page = htmlRe.ReplaceAllString(page, " ")
	page = html.UnescapeString(page)
	lines := strings.Split(page, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(spacesRe.ReplaceAllString(line, " "))
	}
	return strings.TrimSpace(blankLinesRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func toolWebFetchContext(ctx context.Context, args map[string]interface{}) (string, error) {
	u := strArg(args, "url")
	parsed, parseErr := url.Parse(u)
	if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("只支持 http/https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; TG-Agent/2.1)")
	cl := netClientContext(ctx)
	defer cl.CloseIdleConnections()
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (3<<20)+1))
	if readErr != nil {
		return "", readErr
	}
	truncated := len(body) > 3<<20
	if truncated {
		body = body[:3<<20]
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, short(string(body), 200))
	}
	text := strings.ToValidUTF8(string(body), "�")
	title := ""
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") {
		if m := titleRe.FindStringSubmatch(text); m != nil {
			title = oneLine(html.UnescapeString(m[1]), 200)
		}
		text = htmlToText(text)
	}
	if truncated {
		text += "\n…（网页内容已截断）"
	}
	header := fmt.Sprintf("HTTP %d · %s", resp.StatusCode, resp.Request.URL.String())
	if title != "" {
		header += "\n标题: " + title
	}
	return header + "\n\n" + text, nil
}

var (
	ddgResultRe  = regexp.MustCompile(`(?s)class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</(?:a|div|td)>`)
)

// resolveSearchLink unwraps DuckDuckGo redirect links to the real target.
func resolveSearchLink(raw string) string {
	link := html.UnescapeString(raw)
	if strings.HasPrefix(link, "//") {
		link = "https:" + link
	}
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	if strings.HasSuffix(u.Hostname(), "duckduckgo.com") && strings.HasPrefix(u.Path, "/l/") {
		if target := u.Query().Get("uddg"); target != "" {
			return target
		}
	}
	return link
}

// parseSearchResults extracts title, link and snippet for each organic result.
func parseSearchResults(page string, limit int) []searchResult {
	locs := ddgResultRe.FindAllStringSubmatchIndex(page, -1)
	var out []searchResult
	for i, loc := range locs {
		if len(out) >= limit {
			break
		}
		link := resolveSearchLink(page[loc[2]:loc[3]])
		if strings.Contains(link, "duckduckgo.com/y.js") || strings.Contains(link, "ad_domain") {
			continue // sponsored result
		}
		end := len(page)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		r := searchResult{
			Title: oneLine(html.UnescapeString(htmlRe.ReplaceAllString(page[loc[4]:loc[5]], "")), 200),
			URL:   link,
		}
		if m := ddgSnippetRe.FindStringSubmatch(page[loc[1]:end]); m != nil {
			r.Snippet = oneLine(html.UnescapeString(htmlRe.ReplaceAllString(m[1], "")), 300)
		}
		out = append(out, r)
	}
	return out
}

type searchResult struct{ Title, URL, Snippet string }

func toolWebSearchContext(ctx context.Context, args map[string]interface{}) (string, error) {
	q := strArg(args, "query")
	if strings.TrimSpace(q) == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	count := intArg(args, "count", 8, 1, 15)
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(q), nil)
	if reqErr != nil {
		return "", reqErr
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; TG-Agent/2.1)")
	cl := netClientContext(ctx)
	defer cl.CloseIdleConnections()
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("搜索 HTTP %d", resp.StatusCode)
	}
	results := parseSearchResults(string(body), count)
	if len(results) == 0 {
		return "没有解析到搜索结果，可换关键词或用 web_fetch 直接访问网站。\n" + short(htmlToText(string(body)), 800), nil
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return b.String(), nil
}
