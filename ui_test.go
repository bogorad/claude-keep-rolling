package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bogorad/claude-keep-rolling/internal/rolling"
)

// sortFixture has four accounts whose values differ in every column.
func sortFixture(t *testing.T, now time.Time) stateView {
	t.Helper()
	settings := testSettings(t, "providers:\n  meta:\n    enabled: true\n")
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	f := func(v float64) *float64 { return &v }
	return stateView{Now: now, Settings: settings, Providers: providerViews(settings, now), Accounts: []accountView{
		{ID: "a", Name: "alpha", Provider: "claude", Selected: true, Rolling: true, LastPingAt: at(-2 * time.Hour), LastOK: true, WeeklyUtilization: f(.5), LastResetSeen: at(-72 * time.Hour), NextPingAt: at(5 * time.Hour), NextRetryAfter: at(time.Hour)},
		{ID: "b", Name: "Bravo", Provider: "codex", Pinging: true, LastPingAt: at(-time.Hour), WeeklyUtilization: f(.9), LastResetSeen: at(-24 * time.Hour), NextPingAt: at(time.Hour), Unavailable: true},
		{ID: "c", Name: "charlie", Provider: "meta", Selected: true, Rolling: true, Disabled: true, NextPingAt: at(2 * time.Hour)},
		{ID: "d", Name: "delta", Provider: "claude", Selected: true, Rolling: true, LastPingAt: at(-3 * time.Hour), WeeklyReached: true, LastResetSeen: at(-48 * time.Hour), NextRetryAfter: at(-time.Hour)},
	}}
}

func rowIDs(view accountsView) string {
	var out []string
	for _, row := range view.Rows {
		out = append(out, row.ID)
	}
	return strings.Join(out, "")
}

func TestSortEveryColumnBothDirections(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	state := sortFixture(t, now)
	// Empty values: c has no last ping, weekly, reset or cooldown; b and d
	// have no next ping (b is not rolling, d has no time yet); d's cooldown has ended.
	want := map[string][2]string{
		"selected":   {"acdb", "bacd"},
		"name":       {"abcd", "dcba"},
		"provider":   {"adbc", "cbad"},
		"last_ping":  {"dabc", "badc"},
		"weekly":     {"abdc", "dbac"},
		"last_reset": {"adbc", "bdac"},
		"next_ping":  {"cabd", "acbd"},
		"cooldown":   {"abcd", "bacd"},
		"ping":       {"adbc", "cbad"},
	}
	if len(want) != len(sortColumns) {
		t.Fatalf("test covers %d columns, table has %d", len(want), len(sortColumns))
	}
	for _, column := range sortColumns {
		for i, desc := range []bool{false, true} {
			got := rowIDs(buildAccounts(state, sortSpec{Column: column.Key, Desc: desc}))
			if got != want[column.Key][i] {
				t.Errorf("sort %s desc=%v = %s, want %s", column.Key, desc, got, want[column.Key][i])
			}
		}
	}
}

func TestSortTiesUseNameThenID(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	settings := testSettings(t, "")
	state := stateView{Now: now, Settings: settings, Providers: providerViews(settings, now), Accounts: []accountView{
		{ID: "z2", Name: "same", Provider: "claude"},
		{ID: "y", Name: "Beta", Provider: "claude"},
		{ID: "z1", Name: "same", Provider: "claude"},
		{ID: "x", Name: "alpha", Provider: "claude"},
	}}
	for _, desc := range []bool{false, true} {
		// Every account has the same provider and an empty last ping.
		for _, column := range []string{"provider", "last_ping"} {
			if got := rowIDs(buildAccounts(state, sortSpec{Column: column, Desc: desc})); got != "xyz1z2" {
				t.Errorf("ties for %s desc=%v = %s, want xyz1z2", column, desc, got)
			}
		}
	}
}

func TestParseSortFallsBack(t *testing.T) {
	cases := []struct {
		query string
		want  sortSpec
	}{
		{"", sortSpec{Column: "provider"}},
		{"sort=name&dir=desc", sortSpec{Column: "name", Desc: true}},
		{"sort=weekly&dir=asc", sortSpec{Column: "weekly"}},
		{"sort=weekly&dir=sideways", sortSpec{Column: "weekly"}},
		{"sort=bogus&dir=desc", sortSpec{Column: "provider"}},
		{"sort=%3Cscript%3E", sortSpec{Column: "provider"}},
		{"dir=desc", sortSpec{Column: "provider"}},
	}
	for _, tc := range cases {
		q, _ := url.ParseQuery(tc.query)
		if got := parseSort(q); got != tc.want {
			t.Errorf("parseSort(%q) = %+v, want %+v", tc.query, got, tc.want)
		}
	}
}

// fakeHost gives the handlers a fresh roller and a fixed account list.
func fakeHost(t *testing.T, config string, accounts ...tracked) {
	t.Helper()
	oldKeeper, oldSource := keeper, accountSource
	keeper = newRoller()
	keeper.configure(rolling.ParseSettings([]byte(config)))
	accountSource = func() ([]tracked, error) { return accounts, nil }
	t.Cleanup(func() { keeper, accountSource = oldKeeper, oldSource })
}

func named(id, providerID, email string) tracked {
	a := account(id, providerID)
	a.entry.Email = email
	return a
}

func call(t *testing.T, method, path, query, body string, htmx bool) managementResponse {
	t.Helper()
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	req := managementRequest{Method: method, Path: path, Query: q, Body: []byte(body), Headers: http.Header{}}
	if htmx {
		req.Headers.Set("HX-Request", "true")
	}
	return handleManagement(req)
}

func isHTML(t *testing.T, resp managementResponse) string {
	t.Helper()
	if got := resp.Headers.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content type %q, body %s", got, resp.Body)
	}
	return string(resp.Body)
}

func TestTableFragmentHTML(t *testing.T) {
	fakeHost(t, "", named("a", "claude", `<b>x</b>"&'`), named("b", "codex", "b@example.com"))
	resp := call(t, http.MethodGet, tablePath, "sort=name&dir=desc", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body := isHTML(t, resp)
	if strings.Contains(body, "<b>x</b>") || !strings.Contains(body, "&lt;b&gt;x&lt;/b&gt;&#34;&amp;&#39;") {
		t.Fatalf("account name not escaped:\n%s", body)
	}
	// The fragment polls itself with the current sort.
	if !strings.Contains(body, `<div id="accounts" class="accounts" hx-get="../../../management/keep-rolling/table?dir=desc&amp;sort=name" hx-trigger="every 15s [!document.hidden]"`) {
		t.Fatalf("refresh URL does not hold the sort:\n%s", body)
	}
	// The hidden inputs of the row forms carry the sort too.
	if !strings.Contains(body, `name="sort" value="name"`) || !strings.Contains(body, `name="dir" value="desc"`) {
		t.Fatal("row forms do not carry the sort")
	}
	headers := regexp.MustCompile(`<th scope="col" aria-sort="([a-z]+)"><button type="button" class="sort" hx-get="([^"]+)">([^<]+)<span class="arrow" aria-hidden="true">([^<]*)</span>`).FindAllStringSubmatch(body, -1)
	if len(headers) != len(sortColumns) {
		t.Fatalf("found %d sortable headers, want %d", len(headers), len(sortColumns))
	}
	for i, h := range headers {
		column := sortColumns[i]
		if column.Key == "name" {
			if h[1] != "descending" || h[4] != "▼" || !strings.Contains(h[2], "dir=asc&amp;sort=name") {
				t.Fatalf("active header %v", h[1:])
			}
			continue
		}
		if h[1] != "none" || h[4] != "" || !strings.Contains(h[2], "dir=asc&amp;sort="+column.Key) {
			t.Fatalf("header %s: %v", column.Key, h[1:])
		}
	}
	if strings.Index(body, "b@example.com") > strings.Index(body, "&lt;b&gt;") {
		t.Fatal("rows are not sorted by name descending")
	}
	for _, oob := range []string{`id="version" hx-swap-oob="true">v`, `id="error" role="alert" hx-swap-oob="true"></p>`, `id="key-status" hx-swap-oob="true"`} {
		if !strings.Contains(body, oob) {
			t.Fatalf("missing out-of-band element %s", oob)
		}
	}
	if strings.Contains(body, "hx-patch") {
		t.Fatal("a plain table fragment must not write config")
	}
}

func TestTableBadParamsUseDefault(t *testing.T) {
	fakeHost(t, "", named("a", "claude", "a@example.com"))
	resp := call(t, http.MethodGet, tablePath, "sort=%3Cscript%3E&dir=up", "", true)
	body := isHTML(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "table?dir=asc&amp;sort=provider") ||
		!regexp.MustCompile(`aria-sort="ascending"><button[^>]*>Provider`).MatchString(body) || strings.Contains(body, "<script>") {
		t.Fatalf("bad params did not fall back to the default sort:\n%s", body)
	}
}

func TestTableListErrorAndEmpty(t *testing.T) {
	fakeHost(t, "")
	body := isHTML(t, call(t, http.MethodGet, tablePath, "", "", true))
	if !strings.Contains(body, "No supported accounts") {
		t.Fatal("empty state missing")
	}
	accountSource = func() ([]tracked, error) { return nil, &hostError{Message: "host <down>"} }
	body = isHTML(t, call(t, http.MethodGet, tablePath, "", "", true))
	if !strings.Contains(body, "Could not list accounts: host &lt;down&gt;") || strings.Contains(body, "No supported accounts") {
		t.Fatalf("list error not shown:\n%s", body)
	}
}

func TestPingFragment(t *testing.T) {
	off := named("off", "claude", "off@example.com")
	off.entry.Disabled = true
	fakeHost(t, "", named("a", "claude", "a@example.com"), off)

	// No host is loaded in tests, so the ping itself fails and the fragment says so.
	resp := call(t, http.MethodPost, pingPath, "", "id=a&sort=last_ping&dir=desc", true)
	body := isHTML(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<span class="err">Failed just now</span>`) {
		t.Fatalf("ping fragment %d:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "table?dir=desc&amp;sort=last_ping") || resp.Headers.Get("Vary") != "HX-Request" {
		t.Fatal("ping fragment lost the sort or the Vary header")
	}
	for _, tc := range []struct {
		body   string
		status int
		text   string
	}{
		{"sort=name", http.StatusUnprocessableEntity, "Choose an account to ping."},
		{"id=missing", http.StatusUnprocessableEntity, "Ping failed: account not found"},
		{"id=off", http.StatusConflict, "Ping failed: account is disabled"},
	} {
		resp := call(t, http.MethodPost, pingPath, "", tc.body, true)
		if resp.StatusCode != tc.status || !strings.Contains(isHTML(t, resp), tc.text) {
			t.Fatalf("%s: %d\n%s", tc.body, resp.StatusCode, resp.Body)
		}
	}

	// Without HX-Request the route keeps its JSON contract.
	resp = call(t, http.MethodPost, pingPath, "", `{"id":"missing"}`, false)
	if resp.StatusCode != http.StatusNotFound || resp.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("JSON ping: %d %s", resp.StatusCode, resp.Body)
	}
	resp = call(t, http.MethodPost, pingPath, "", `{"id":"a"}`, false)
	var state stateView
	if err := json.Unmarshal(resp.Body, &state); err != nil || resp.StatusCode != http.StatusOK || len(state.Accounts) != 2 {
		t.Fatalf("JSON ping: %d %v %s", resp.StatusCode, err, resp.Body)
	}
}

func settingsForm(overrides map[string]string) string {
	form := url.Values{"prompt": {"hi"}, "delay_min": {"1m"}, "delay_max": {"10m"}, "stagger": {"2m"}, "meta_interval": {"1h"}, "cooldown_reset": {"on"}}
	for k, v := range overrides {
		if v == "" {
			form.Del(k)
		} else {
			form.Set(k, v)
		}
	}
	return form.Encode()
}

func TestSettingsValidation(t *testing.T) {
	fakeHost(t, "")
	for _, tc := range []struct {
		field, value, text string
	}{
		{"meta_interval", "4m", "meta_interval 4m0s is outside 5m0s to 24h0m0s."},
		{"meta_interval", "25h", "meta_interval 25h0m0s is outside 5m0s to 24h0m0s."},
		{"meta_interval", "soon", "&#34;soon&#34; is not a duration"},
		{"delay_max", "30s", "delay_max 30s is below delay_min 1m0s."},
		{"stagger", "2h", "stagger 2h0m0s is outside 0s to 1h0m0s."},
	} {
		resp := call(t, http.MethodPost, settingsPath, "", settingsForm(map[string]string{tc.field: tc.value}), true)
		body := isHTML(t, resp)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, tc.text) {
			t.Fatalf("%s=%s: %d\n%s", tc.field, tc.value, resp.StatusCode, body)
		}
		if !strings.Contains(body, `name="`+tc.field+`" value="`+tc.value+`" aria-invalid="true" aria-describedby="error-`+tc.field+`"`) {
			t.Fatalf("%s: the submitted value is not shown as invalid:\n%s", tc.field, body)
		}
		if strings.Contains(body, "hx-patch") {
			t.Fatal("an invalid form must not write config")
		}
	}

	resp := call(t, http.MethodPost, settingsPath, "", settingsForm(nil), true)
	if body := isHTML(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "No changes.") || strings.Contains(body, "hx-patch") {
		t.Fatalf("unchanged settings: %d\n%s", resp.StatusCode, body)
	}

	resp = call(t, http.MethodPost, settingsPath, "", settingsForm(map[string]string{"meta_interval": "30m", "cooldown_reset": ""}), true)
	body := isHTML(t, resp)
	want := `<div id="write" hidden hx-swap-oob="true"><div hx-patch="../../../management/plugins/keep-rolling/config" hx-ext="json-enc" hx-vals="{&#34;cooldown_reset&#34;:false,&#34;meta_interval&#34;:&#34;30m0s&#34;}" hx-trigger="load" hx-swap="none"></div></div>`
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, want) || !strings.Contains(body, `name="meta_interval" value="30m0s"`) {
		t.Fatalf("valid settings: %d\n%s", resp.StatusCode, body)
	}
}

func patchOf(t *testing.T, body string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`hx-vals="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no config write in:\n%s", body)
	}
	raw := strings.NewReplacer("&#34;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'").Replace(m[1])
	var patch map[string]any
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		t.Fatalf("patch %s: %v", raw, err)
	}
	return patch
}

func TestProviderChange(t *testing.T) {
	fakeHost(t, "")
	resp := call(t, http.MethodPost, providerPath, "", "provider=meta&field=enabled&enabled=on", true)
	body := isHTML(t, resp)
	providers := patchOf(t, body)["providers"].(map[string]any)
	if resp.StatusCode != http.StatusOK || len(providers) != 5 || providers["meta"].(map[string]any)["enabled"] != true || providers["claude"].(map[string]any)["enabled"] != true {
		t.Fatalf("enable meta: %d %v", resp.StatusCode, providers)
	}
	if !strings.Contains(body, `aria-label="Enable Meta" checked`) {
		t.Fatal("the fragment does not show the change")
	}
	for _, form := range []string{"provider=gemini&field=enabled", "provider=claude&field=colour", "provider=claude&field=model&model=two+words", "provider=xai&field=model&model=grok"} {
		resp := call(t, http.MethodPost, providerPath, "", form, true)
		if body := isHTML(t, resp); resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, "hx-patch") {
			t.Fatalf("%s: %d\n%s", form, resp.StatusCode, body)
		}
	}
	resp = call(t, http.MethodPost, providerPath, "", "provider=claude&field=model&model=", true)
	if body := isHTML(t, resp); !strings.Contains(body, "No changes.") {
		t.Fatalf("an empty model means the default model:\n%s", body)
	}
}

func TestSelectAccount(t *testing.T) {
	off := named("off", "claude", "off@example.com")
	off.entry.Disabled = true
	fakeHost(t, "", named("a", "claude", "a@example.com"), named("b", "claude", "b@example.com"), named("c", "codex", "c@example.com"), off)

	resp := call(t, http.MethodPost, selectPath, "", "account=a&sort=name&dir=asc", true)
	body := isHTML(t, resp)
	claude := patchOf(t, body)["providers"].(map[string]any)["claude"].(map[string]any)
	if resp.StatusCode != http.StatusOK || claude["select_all"] != false || !slices.Equal(toStrings(claude["accounts"]), []string{"b", "off"}) {
		t.Fatalf("turning a off: %d %v", resp.StatusCode, claude)
	}
	if !strings.Contains(body, `aria-label="Keep a@example.com rolling">`) || !strings.Contains(body, `aria-label="Keep b@example.com rolling" checked>`) {
		t.Fatalf("the table does not show the new selection:\n%s", body)
	}
	if resp := call(t, http.MethodPost, selectPath, "", "account=a&on=1", true); strings.Contains(string(resp.Body), "hx-patch") {
		t.Fatal("selecting an account that is already selected must not write")
	}
	if resp := call(t, http.MethodPost, selectPath, "", "account=off", true); resp.StatusCode != http.StatusConflict {
		t.Fatalf("disabled account: %d", resp.StatusCode)
	}
	if resp := call(t, http.MethodPost, selectPath, "", "account=nobody", true); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown account: %d", resp.StatusCode)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestAssetRoutes(t *testing.T) {
	for _, tc := range []struct{ path, sha string }{
		{resourceBase + htmxPath, htmxSHA256},
		{resourceBase + jsonEncPath, jsonEncSHA256},
	} {
		resp := call(t, http.MethodGet, tc.path, "", "", false)
		sum := sha256.Sum256(resp.Body)
		if resp.StatusCode != http.StatusOK || resp.Headers.Get("Content-Type") != "text/javascript; charset=utf-8" || hex.EncodeToString(sum[:]) != tc.sha {
			t.Fatalf("%s: %d %q sha256 %x", tc.path, resp.StatusCode, resp.Headers.Get("Content-Type"), sum)
		}
	}
	if !strings.Contains(string(htmxJS), `version:"`+htmxVersion+`"`) {
		t.Fatal("embedded htmx does not report its version")
	}
	raw, err := handleMethod("management.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{`"Path":"/status","Menu":"Keep Rolling"`, `"Path":"` + htmxPath + `","Menu":""`, `"Path":"` + jsonEncPath + `","Menu":""`, tablePath, viewPath, settingsPath, providerPath, selectPath} {
		if !strings.Contains(string(raw), path) {
			t.Fatalf("registration lacks %s: %s", path, raw)
		}
	}
}

func TestStatusPage(t *testing.T) {
	resp := call(t, http.MethodGet, resourceBase+statusPath, "", "", false)
	body := isHTML(t, resp)
	for _, want := range []string{
		`<script src="assets/htmx-2.0.11.min.js" integrity="` + attrEscaped(subresourceIntegrity(htmxJS)) + `"></script>`,
		`<script src="assets/json-enc-2.0.3.js" integrity="` + attrEscaped(subresourceIntegrity(jsonEncJS)) + `"></script>`,
		`<meta name="htmx-config"`, `"selfRequestsOnly":true`, `"historyCacheSize":0`,
		`hx-get="../../../management/keep-rolling/view" hx-target="#app"`,
		`<input type="password" id="key" autocomplete="off"`,
		`<div id="app"></div>`, `<div id="write" hidden></div>`, `<noscript>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %s", want)
		}
	}
	if n := strings.Count(body, "<script>"); n != 1 || !strings.Contains(body, `addEventListener("htmx:configRequest"`) {
		t.Fatalf("want exactly one inline script, the key listener; found %d", n)
	}
	for _, banned := range []string{"localStorage", "sessionStorage", `name="key"`, "fetch(", "cdn", "unpkg"} {
		if strings.Contains(body, banned) {
			t.Fatalf("page contains %q", banned)
		}
	}
}

func TestViewFragment(t *testing.T) {
	fakeHost(t, "meta_interval: 2h\n", named("a", "claude", "a@example.com"))
	resp := call(t, http.MethodGet, viewPath, "sort=weekly&dir=desc", "", true)
	body := isHTML(t, resp)
	for _, want := range []string{`<div id="accounts"`, "table?dir=desc&amp;sort=weekly", `<div id="providers"`, `<div id="settings">`, `name="meta_interval" value="2h0m0s"`, "pinged every 2h0m0s", `id="key-status" hx-swap-oob="true"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("view lacks %s:\n%s", want, body)
		}
	}
}

func TestManagementRequestCarriesHeadersAndQuery(t *testing.T) {
	// The host sends pluginapi.ManagementRequest without JSON tags.
	raw := `{"Method":"GET","Path":"/v0/management/keep-rolling/table","Headers":{"Hx-Request":["true"]},"Query":{"sort":["cooldown"],"dir":["desc"]},"Body":null}`
	var req managementRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if !isHTMX(req) || parseSort(req.Query) != (sortSpec{Column: "cooldown", Desc: true}) {
		t.Fatalf("decoded %+v", req)
	}
}

// attrEscaped is how html/template writes a base64 value into an attribute.
func attrEscaped(s string) string { return strings.ReplaceAll(s, "+", "&#43;") }

func TestPageHTMXConfig(t *testing.T) {
	body := string(renderPage().Body)
	start := strings.Index(body, `content='`) + len(`content='`)
	var config struct {
		SelfRequestsOnly, AllowScriptTags, HistoryEnabled bool
		HistoryCacheSize                                  int
		ResponseHandling                                  []map[string]any
	}
	if err := json.Unmarshal([]byte(body[start:start+strings.Index(body[start:], `'`)]), &config); err != nil {
		t.Fatal(err)
	}
	// htmx history would copy fragments to localStorage; it is off.
	if !config.SelfRequestsOnly || config.AllowScriptTags || config.HistoryEnabled || config.HistoryCacheSize != 0 || len(config.ResponseHandling) != 5 {
		t.Fatalf("htmx config %+v", config)
	}
}
