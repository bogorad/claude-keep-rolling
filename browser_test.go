package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBrowserFlow drives the real page in headless Chromium against a fake
// host that checks the management key like CLIProxyAPI does. It runs only
// when KEEP_ROLLING_BROWSER names a Chromium binary (docs/ui.md).
func TestBrowserFlow(t *testing.T) {
	browser := os.Getenv("KEEP_ROLLING_BROWSER")
	if browser == "" {
		t.Skip("set KEEP_ROLLING_BROWSER to a Chromium binary to run the browser test")
	}
	fakeHost(t, "", named("a", "claude", "a@example.com"), named("a2", "claude", "a2@example.com"), named("b", "codex", "b@example.com"))

	type seen struct {
		Method, Path, Query, Key, ContentType, Body string
	}
	var mu sync.Mutex
	var requests []seen
	// The driver script exists only in this test. It types a wrong key, then
	// the right one, sorts by account, submits a bad and a good Meta interval,
	// and turns one account off.
	driver := `<script>
var step=function(ms,f){setTimeout(f,ms)};
step(300,function(){document.getElementById("key").value="wrong";document.querySelector(".keybox button").click()});
step(1300,function(){document.getElementById("key").value="k";document.querySelector(".keybox button").click()});
step(2300,function(){Array.from(document.querySelectorAll("button.sort")).find(function(b){return b.textContent.indexOf("Account")===0}).click()});
step(3300,function(){document.querySelector("input[name=meta_interval]").value="1m";document.querySelector("#settings button[type=submit]").click()});
step(4300,function(){document.querySelector("input[name=meta_interval]").value="30m";document.querySelector("#settings button[type=submit]").click()});
step(5300,function(){document.querySelector("input[aria-label='Keep a@example.com rolling']").click()});
</script></body>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Management-Key"), r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		write := func(resp managementResponse) {
			for k, v := range resp.Headers {
				w.Header()[k] = v
			}
			if resp.StatusCode == 0 {
				resp.StatusCode = http.StatusOK
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(resp.Body)
		}
		req := managementRequest{Method: r.Method, Path: r.URL.Path, Headers: r.Header, Query: r.URL.Query(), Body: body}
		switch {
		case strings.HasPrefix(r.URL.Path, resourceBase+"/"):
			resp := handleManagement(req)
			if r.URL.Path == resourceBase+statusPath {
				resp.Body = bytes.Replace(resp.Body, []byte("</body>"), []byte(driver), 1)
			}
			write(resp)
		case r.Header.Get("X-Management-Key") != "k":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid management key"}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v0/management/plugins/"+pluginID+"/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			write(handleManagement(req))
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, browser, "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run",
		"--user-data-dir="+t.TempDir(), "--virtual-time-budget=9000", "--dump-dom", server.URL+resourceBase+statusPath).Output()
	if err != nil {
		t.Fatalf("chromium: %v", err)
	}
	dom := string(out)

	mu.Lock()
	defer mu.Unlock()
	var log strings.Builder
	for _, r := range requests {
		log.WriteString(r.Method + " " + r.Path + "?" + r.Query + " key=" + r.Key + " ct=" + r.ContentType + " body=" + r.Body + "\n")
	}
	find := func(method, path string, ok func(seen) bool) bool {
		for _, r := range requests {
			if r.Method == method && r.Path == path && ok(r) {
				return true
			}
		}
		return false
	}
	anyReq := func(seen) bool { return true }
	checks := []struct {
		name string
		ok   bool
	}{
		{"htmx and json-enc load (SRI passes)", find(http.MethodGet, resourceBase+htmxPath, anyReq) && find(http.MethodGet, resourceBase+jsonEncPath, anyReq)},
		{"the wrong key is sent and rejected", find(http.MethodGet, viewPath, func(r seen) bool { return r.Key == "wrong" })},
		{"the view loads with the key header", find(http.MethodGet, viewPath, func(r seen) bool { return r.Key == "k" })},
		{"the sort header requests the sorted table", find(http.MethodGet, tablePath, func(r seen) bool { return r.Query == "dir=asc&sort=name" && r.Key == "k" })},
		{"the settings form posts to the server", find(http.MethodPost, settingsPath, func(r seen) bool { return strings.Contains(r.Body, "meta_interval=1m") })},
		{"the valid change is patched as JSON", find(http.MethodPatch, "/v0/management/plugins/"+pluginID+"/config", func(r seen) bool {
			return r.Key == "k" && r.ContentType == "application/json" && r.Body == `{"meta_interval":"30m0s"}`
		})},
		{"the checkbox posts the selection with the sort", find(http.MethodPost, selectPath, func(r seen) bool {
			return strings.Contains(r.Body, "account=a") && strings.Contains(r.Body, "sort=name") && !strings.Contains(r.Body, "on=1")
		})},
		{"the account selection is patched as JSON", find(http.MethodPatch, "/v0/management/plugins/"+pluginID+"/config", func(r seen) bool {
			return strings.Contains(r.Body, `"claude":{"accounts":["a2"],"enabled":true,"model":"claude-haiku-4-5-20251001","select_all":false}`)
		})},
		{"the key is never a form field", !strings.Contains(log.String(), "key=wrong&") && !strings.Contains(log.String(), "body=key")},
		{"the table polls with the sort", strings.Contains(dom, `hx-get="../../../management/keep-rolling/table?dir=asc&amp;sort=name"`)},
		{"the active header shows its arrow", strings.Contains(dom, `aria-sort="ascending"><button type="button" class="sort" hx-get="../../../management/keep-rolling/table?dir=desc&amp;sort=name">Account<span class="arrow" aria-hidden="true">▲</span>`)},
		{"the saved value is shown", strings.Contains(dom, `name="meta_interval" value="30m0s"`)},
		{"the page shows it is connected", strings.Contains(dom, `<span id="key-status">Connected</span>`)},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s", c.name)
		}
	}
	if t.Failed() {
		t.Logf("requests:\n%s", log.String())
		t.Logf("dom:\n%s", dom)
	}
}
