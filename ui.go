package main

// The status page is server-rendered HTML driven by htmx (docs/ui.md). The
// resource route serves the page shell without data. The authenticated
// management routes return HTML fragments. Sorting, validation and the
// config patch are computed here; the browser only sends requests.

import (
	"bytes"
	"crypto/sha512"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bogorad/claude-keep-rolling/internal/provider"
	"github.com/bogorad/claude-keep-rolling/internal/rolling"
)

// Vendored htmx, unmodified, from the npm registry (tarball integrity checked):
//
//	htmx.org@2.0.11 dist/htmx.min.js       sha256 d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717
//	htmx-ext-json-enc@2.0.3 json-enc.js    sha256 9fe14faaedb792c70c187e219b2d8e72fed92891375205dad9dfefe18d9accea
//
// Both are Zero-Clause BSD; the notices are in assets/htmx/.
const (
	htmxVersion    = "2.0.11"
	htmxSHA256     = "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717"
	jsonEncVersion = "2.0.3"
	jsonEncSHA256  = "9fe14faaedb792c70c187e219b2d8e72fed92891375205dad9dfefe18d9accea"
)

//go:embed assets/htmx/htmx.min.js
var htmxJS []byte

//go:embed assets/htmx/json-enc.js
var jsonEncJS []byte

//go:embed templates/*.html
var templateFS embed.FS

var pageTemplates = template.Must(template.New("").ParseFS(templateFS, "templates/*.html"))

const (
	resourceBase = "/v0/resource/plugins/" + pluginID
	// The asset paths carry the release, so a browser may cache them for good.
	htmxPath    = "/assets/htmx-" + htmxVersion + ".min.js"
	jsonEncPath = "/assets/json-enc-" + jsonEncVersion + ".js"

	// URLs in the page and its fragments are relative to the page
	// (<prefix>/v0/resource/plugins/keep-rolling/status), so they keep any
	// reverse-proxy prefix.
	pageToManagement = "../../../management/" + pluginID
	pageToConfig     = "../../../management/plugins/" + pluginID + "/config"

	// pollEvery is the refresh interval of the accounts fragment.
	pollEvery = "15s"
)

// accountSource lists the host accounts for the page. Tests replace it.
var accountSource = listAccounts

func htmlResponse(status int, body []byte) managementResponse {
	return managementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           {"text/html; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"Vary":                   {"HX-Request"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body,
	}
}

func render(status int, name string, data any) managementResponse {
	var buf bytes.Buffer
	if err := pageTemplates.ExecuteTemplate(&buf, name, data); err != nil {
		return managementResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    http.Header{"Content-Type": {"text/plain; charset=utf-8"}, "Cache-Control": {"no-store"}},
			Body:       []byte("render " + name + ": " + err.Error()),
		}
	}
	return htmlResponse(status, buf.Bytes())
}

func assetResponse(body []byte) managementResponse {
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":           {"text/javascript; charset=utf-8"},
			"Cache-Control":          {"public, max-age=31536000, immutable"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: body,
	}
}

// isHTMX reports a request sent by htmx. Only htmx sends HX-Request.
func isHTMX(req managementRequest) bool {
	return req.Headers.Get("HX-Request") == "true"
}

// formValues reads a form-encoded POST body. A body that is not a form gives no values.
func formValues(req managementRequest) url.Values {
	values, err := url.ParseQuery(string(req.Body))
	if err != nil {
		return url.Values{}
	}
	return values
}

func subresourceIntegrity(body []byte) string {
	sum := sha512.Sum384(body)
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

// ---- Page shell ----

type pageView struct {
	HTMXPath, HTMXIntegrity       string
	JSONEncPath, JSONEncIntegrity string
	ViewURL                       string
}

func renderPage() managementResponse {
	return render(http.StatusOK, "page", pageView{
		HTMXPath:         strings.TrimPrefix(htmxPath, "/"),
		HTMXIntegrity:    subresourceIntegrity(htmxJS),
		JSONEncPath:      strings.TrimPrefix(jsonEncPath, "/"),
		JSONEncIntegrity: subresourceIntegrity(jsonEncJS),
		ViewURL:          pageToManagement + "/view",
	})
}

// ---- Sorting ----

type sortColumn struct {
	Key   string
	Label string
	value func(row accountRow) sortValue
}

// sortValue is the key of one cell. Empty values sort last in both directions.
type sortValue struct {
	empty bool
	text  string
	num   float64
}

func textValue(s string) sortValue { return sortValue{empty: s == "", text: strings.ToLower(s)} }
func numValue(n float64) sortValue { return sortValue{num: n} }
func timeValue(t *time.Time) sortValue {
	if t == nil {
		return sortValue{empty: true}
	}
	return sortValue{num: float64(t.UnixNano())}
}

const defaultSortColumn = "provider"

// sortColumns lists every column of the accounts table, in display order.
var sortColumns = []sortColumn{
	{"selected", "Keep", func(r accountRow) sortValue {
		// Ascending puts the accounts kept rolling first.
		if r.Selected {
			return numValue(0)
		}
		return numValue(1)
	}},
	{"name", "Account", func(r accountRow) sortValue { return textValue(r.Name) }},
	{"provider", "Provider", func(r accountRow) sortValue { return textValue(r.ProviderName) }},
	{"last_ping", "Last ping", func(r accountRow) sortValue { return timeValue(r.view.LastPingAt) }},
	{"weekly", "Weekly", func(r accountRow) sortValue {
		switch {
		case r.view.WeeklyUtilization != nil:
			return numValue(*r.view.WeeklyUtilization)
		case r.view.WeeklyReached:
			return numValue(1)
		}
		return sortValue{empty: true}
	}},
	{"last_reset", "Last reset seen", func(r accountRow) sortValue { return timeValue(r.view.LastResetSeen) }},
	{"next_ping", "Next ping", func(r accountRow) sortValue {
		if !r.Rolling {
			return sortValue{empty: true}
		}
		return timeValue(r.view.NextPingAt)
	}},
	{"cooldown", "Cooldown", func(r accountRow) sortValue {
		switch {
		case r.coolingUntil != nil:
			return timeValue(r.coolingUntil)
		case r.view.Unavailable:
			// Unavailable without an end time sorts after every timed cooldown.
			return numValue(math.MaxFloat64)
		}
		return sortValue{empty: true}
	}},
	{"ping", "Ping", func(r accountRow) sortValue {
		// Ascending: ready to ping, then pinging, then no ping possible.
		switch {
		case r.CanPingNow:
			return numValue(0)
		case r.Pinging:
			return numValue(1)
		}
		return numValue(2)
	}},
}

func findColumn(key string) (sortColumn, bool) {
	for _, column := range sortColumns {
		if column.Key == key {
			return column, true
		}
	}
	return sortColumn{}, false
}

type sortSpec struct {
	Column string
	Desc   bool
}

func (s sortSpec) Dir() string {
	if s.Desc {
		return "desc"
	}
	return "asc"
}

// parseSort reads sort and dir. An unknown column gives the default sort; an
// unknown dir gives ascending. Neither is an error.
func parseSort(values url.Values) sortSpec {
	column := values.Get("sort")
	if _, ok := findColumn(column); !ok {
		return sortSpec{Column: defaultSortColumn}
	}
	return sortSpec{Column: column, Desc: values.Get("dir") == "desc"}
}

func tableURL(spec sortSpec) string {
	return pageToManagement + "/table?" + url.Values{"sort": {spec.Column}, "dir": {spec.Dir()}}.Encode()
}

func compareValues(a, b sortValue) int {
	if a.text != "" || b.text != "" {
		return strings.Compare(a.text, b.text)
	}
	switch {
	case a.num < b.num:
		return -1
	case a.num > b.num:
		return 1
	}
	return 0
}

// sortRows sorts in place. Empty values go last in both directions. Ties
// fall back to the account name, then the account ID, both ascending.
func sortRows(rows []accountRow, spec sortSpec) {
	column, ok := findColumn(spec.Column)
	if !ok {
		column, _ = findColumn(defaultSortColumn)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := column.value(rows[i]), column.value(rows[j])
		switch {
		case a.empty && !b.empty:
			return false
		case !a.empty && b.empty:
			return true
		case !a.empty && !b.empty:
			if c := compareValues(a, b); c != 0 {
				if spec.Desc {
					return c > 0
				}
				return c < 0
			}
		}
		if c := strings.Compare(strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)); c != 0 {
			return c < 0
		}
		return rows[i].ID < rows[j].ID
	})
}

type headerCell struct {
	Label    string
	AriaSort string
	Arrow    string
	URL      string
}

func headerCells(spec sortSpec) []headerCell {
	cells := make([]headerCell, 0, len(sortColumns))
	for _, column := range sortColumns {
		cell := headerCell{Label: column.Label, AriaSort: "none"}
		next := sortSpec{Column: column.Key}
		if column.Key == spec.Column {
			next.Desc = !spec.Desc
			cell.AriaSort, cell.Arrow = "ascending", "▲"
			if spec.Desc {
				cell.AriaSort, cell.Arrow = "descending", "▼"
			}
		}
		cell.URL = tableURL(next)
		cells = append(cells, cell)
	}
	return cells
}

// ---- Accounts fragment ----

type cellView struct {
	Text  string
	Class string
	Title string
	Notes []string
}

type accountRow struct {
	ID, Name, ProviderName      string
	Disabled, Selected, Rolling bool
	Pinging, CanPingNow         bool
	ProviderNote                string
	LastPing, Weekly, LastReset cellView
	NextPing, Cooldown          cellView
	view                        accountView
	coolingUntil                *time.Time
}

type accountsView struct {
	Spec       sortSpec
	RefreshURL string
	PingURL    string
	SelectURL  string
	Poll       string
	Headers    []headerCell
	Rows       []accountRow
	Notice     string
	NoticeErr  bool
	Listed     bool
}

// statusView is the page chrome that fragments update out of band.
type statusView struct {
	Version string
	Error   string
}

type accountsFragment struct {
	Accounts accountsView
	Status   statusView
	Write    *writeView
}

func relTime(t, now time.Time) string {
	d := t.Sub(now)
	mins := int(math.Round(math.Abs(float64(d)) / float64(time.Minute)))
	if mins < 1 {
		if d >= 0 {
			return "in under a minute"
		}
		return "just now"
	}
	var text string
	switch {
	case mins < 60:
		text = fmt.Sprintf("%dm", mins)
	case mins < 1440:
		text = fmt.Sprintf("%dh %dm", mins/60, mins%60)
	default:
		text = fmt.Sprintf("%dd %dh", mins/1440, mins%1440/60)
	}
	if d >= 0 {
		return "in " + text
	}
	return text + " ago"
}

// absTime is a 24-hour UTC time for titles.
func absTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

func timeCell(t *time.Time, now time.Time, empty string) cellView {
	if t == nil {
		return cellView{Text: empty, Class: "muted"}
	}
	return cellView{Text: relTime(*t, now), Title: absTime(*t)}
}

func buildRow(acc accountView, info providerView, enabled bool, now time.Time) accountRow {
	row := accountRow{
		ID: acc.ID, Name: acc.Name, ProviderName: info.Name,
		Disabled: acc.Disabled, Selected: acc.Selected, Rolling: acc.Rolling, Pinging: acc.Pinging,
		CanPingNow: !acc.Disabled && !acc.Pinging && info.CanPing,
		view:       acc,
	}
	if row.ProviderName == "" {
		row.ProviderName = acc.Provider
	}
	switch {
	case !enabled:
		row.ProviderNote = "provider off"
	case !info.CanPing:
		row.ProviderNote = "no pings"
	}

	switch {
	case acc.Pinging:
		row.LastPing = cellView{Text: "Pinging..."}
	case acc.LastPingAt != nil:
		row.LastPing = cellView{Text: "Failed " + relTime(*acc.LastPingAt, now), Class: "err", Title: absTime(*acc.LastPingAt)}
		if acc.LastOK {
			row.LastPing.Text, row.LastPing.Class = "OK "+relTime(*acc.LastPingAt, now), "ok"
		}
		for _, note := range []string{acc.LastError, acc.LastResult} {
			if note != "" {
				row.LastPing.Notes = append(row.LastPing.Notes, note)
			}
		}
	default:
		row.LastPing = cellView{Text: "Never", Class: "muted"}
	}

	switch {
	case acc.WeeklyUtilization != nil:
		pct := int(math.Round(*acc.WeeklyUtilization * 100))
		if *acc.WeeklyUtilization < 1 {
			pct = min(pct, 99)
		}
		row.Weekly = cellView{Text: fmt.Sprintf("%d%% used", pct)}
		if acc.WeeklyReached {
			row.Weekly.Class = "err"
		}
	case acc.WeeklyReached:
		row.Weekly = cellView{Text: "Limit reached", Class: "err"}
	default:
		row.Weekly = cellView{Text: "Unknown", Class: "muted"}
	}
	switch {
	case acc.WeeklyResetAt != nil:
		row.Weekly.Notes = append(row.Weekly.Notes, "Resets "+relTime(*acc.WeeklyResetAt, now))
		row.Weekly.Title = absTime(*acc.WeeklyResetAt)
	case info.FixedResetAt != nil:
		row.Weekly.Notes = append(row.Weekly.Notes, "Fixed reset "+relTime(*info.FixedResetAt, now))
		row.Weekly.Title = absTime(*info.FixedResetAt)
	}

	row.LastReset = timeCell(acc.LastResetSeen, now, "None yet")
	row.NextPing = cellView{Text: "Not rolling", Class: "muted"}
	if acc.Rolling {
		row.NextPing = timeCell(acc.NextPingAt, now, "Soon")
	}

	switch {
	case acc.NextRetryAfter != nil && acc.NextRetryAfter.After(now):
		row.coolingUntil = acc.NextRetryAfter
		row.Cooldown = cellView{Text: "Until " + relTime(*acc.NextRetryAfter, now), Class: "err", Title: absTime(*acc.NextRetryAfter)}
	case acc.Unavailable:
		row.Cooldown = cellView{Text: "Unavailable", Class: "err"}
	default:
		row.Cooldown = cellView{Text: "None", Class: "muted"}
	}
	if acc.CooldownResetAt != nil {
		row.Cooldown.Notes = append(row.Cooldown.Notes, "Cleared "+relTime(*acc.CooldownResetAt, now))
	}
	return row
}

func buildAccounts(state stateView, spec sortSpec) accountsView {
	view := accountsView{
		Spec:       spec,
		RefreshURL: tableURL(spec),
		PingURL:    pageToManagement + "/ping",
		SelectURL:  pageToManagement + "/select",
		Poll:       pollEvery,
		Headers:    headerCells(spec),
		Listed:     state.ListError == "",
	}
	infos := map[string]providerView{}
	for _, info := range state.Providers {
		infos[info.ID] = info
	}
	for _, acc := range state.Accounts {
		view.Rows = append(view.Rows, buildRow(acc, infos[acc.Provider], state.Settings.Provider(acc.Provider).Enabled, state.Now))
	}
	sortRows(view.Rows, spec)
	return view
}

func statusOf(state stateView) statusView {
	status := statusView{Version: state.Version}
	switch {
	case state.ListError != "":
		status.Error = "Could not list accounts: " + state.ListError
	case state.ConfigError != "":
		status.Error = "The plugin config is invalid, so the previous settings are still in use: " + state.ConfigError
	}
	return status
}

func renderAccounts(status int, state stateView, spec sortSpec, notice string, noticeErr bool, write *writeView) managementResponse {
	accounts := buildAccounts(state, spec)
	accounts.Notice, accounts.NoticeErr = notice, noticeErr
	return render(status, "accounts-fragment", accountsFragment{Accounts: accounts, Status: statusOf(state), Write: write})
}

func handleTable(req managementRequest) managementResponse {
	return renderAccounts(http.StatusOK, buildState(time.Now()), parseSort(req.Query), "", false, nil)
}

// handlePingHTML runs a manual ping for an htmx form and returns the accounts
// fragment with the result. Fragment errors use 409 and 422 (docs/ui.md).
func handlePingHTML(req managementRequest) managementResponse {
	form := formValues(req)
	spec := parseSort(form)
	id := form.Get("id")
	if id == "" {
		return renderAccounts(http.StatusUnprocessableEntity, buildState(time.Now()), spec, "Choose an account to ping.", true, nil)
	}
	status, message := runManualPing(id)
	switch status {
	case http.StatusOK:
		return renderAccounts(http.StatusOK, buildState(time.Now()), spec, "", false, nil)
	case http.StatusNotFound:
		status = http.StatusUnprocessableEntity
	case http.StatusBadGateway:
		// The listing failure shows as the page error; the table stays.
		status = http.StatusOK
	}
	return renderAccounts(status, buildState(time.Now()), spec, "Ping failed: "+message, true, nil)
}

// ---- Config writes ----

// writeView is a validated config patch. The page sends it to the host
// plugin-config route (PATCH, JSON body via json-enc), because the plugin
// cannot write the host config itself. The host reloads the plugin, which
// validates the config again with rolling.ParseSettings.
type writeView struct {
	URL  string
	JSON string
}

func newWrite(patch map[string]any) (*writeView, error) {
	raw, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	return &writeView{URL: pageToConfig, JSON: string(raw)}, nil
}

func cloneSettings(s rolling.Settings) rolling.Settings {
	out := s
	out.Providers = make(map[string]rolling.ProviderSettings, len(s.Providers))
	for id, p := range s.Providers {
		p.Accounts = append([]string{}, p.Accounts...)
		out.Providers[id] = p
	}
	return out
}

// providersPatch is the whole providers object; the host merges top-level keys only.
func providersPatch(s rolling.Settings) map[string]any {
	providers := map[string]any{}
	for id, p := range s.Providers {
		providers[id] = map[string]any{"enabled": p.Enabled, "model": p.Model, "select_all": p.SelectAll, "accounts": append([]string{}, p.Accounts...)}
	}
	return map[string]any{"providers": providers}
}

// ---- Settings fragment ----

type settingsField struct {
	Name, Label, Value, Hint, Error string
}

type settingsView struct {
	URL           string
	Fields        []settingsField
	CooldownReset bool
	Notice        string
	NoticeErr     bool
	Write         *writeView
}

var settingsLabels = []struct{ name, label, hint string }{
	{"prompt", "Prompt", "Empty means " + rolling.DefaultPrompt + "."},
	{"delay_min", "Delay after reset, min", "At least " + rolling.MinDelay.String() + "."},
	{"delay_max", "Delay after reset, max", "At most " + rolling.MaxDelay.String() + "."},
	{"stagger", "Stagger after start", "0s to " + rolling.MaxStagger.String() + "."},
	{"meta_interval", "Meta ping interval", rolling.MinMetaInterval.String() + " to " + rolling.MaxMetaInterval.String() + "."},
}

func settingsValues(s rolling.Settings) map[string]string {
	return map[string]string{
		"prompt":        s.Prompt,
		"delay_min":     time.Duration(s.DelayMin).String(),
		"delay_max":     time.Duration(s.DelayMax).String(),
		"stagger":       time.Duration(s.Stagger).String(),
		"meta_interval": time.Duration(s.MetaInterval).String(),
	}
}

func buildSettings(values map[string]string, cooldownReset bool, errs map[string]string) settingsView {
	view := settingsView{URL: pageToManagement + "/settings", CooldownReset: cooldownReset}
	for _, f := range settingsLabels {
		view.Fields = append(view.Fields, settingsField{Name: f.name, Label: f.label, Value: values[f.name], Hint: f.hint, Error: errs[f.name]})
	}
	return view
}

// applySettingsForm validates the form against the current settings. It
// returns the new settings, the submitted values for redisplay, and the
// field errors.
func applySettingsForm(form url.Values, current rolling.Settings) (rolling.Settings, map[string]string, map[string]string) {
	defaults, _ := rolling.ParseSettings(nil)
	next := cloneSettings(current)
	values := map[string]string{}
	errs := map[string]string{}

	prompt := strings.TrimSpace(form.Get("prompt"))
	if prompt == "" {
		prompt = defaults.Prompt
	}
	values["prompt"] = prompt
	next.Prompt = prompt

	durations := []struct {
		name   string
		target *rolling.Duration
		def    rolling.Duration
	}{
		{"delay_min", &next.DelayMin, defaults.DelayMin},
		{"delay_max", &next.DelayMax, defaults.DelayMax},
		{"stagger", &next.Stagger, defaults.Stagger},
		{"meta_interval", &next.MetaInterval, defaults.MetaInterval},
	}
	for _, d := range durations {
		raw := strings.TrimSpace(form.Get(d.name))
		values[d.name] = raw
		if raw == "" {
			*d.target = d.def
			values[d.name] = time.Duration(d.def).String()
			continue
		}
		parsed, err := rolling.ParseDuration(raw)
		if err != nil {
			errs[d.name] = fmt.Sprintf("%q is not a duration such as 90s, 10m or 1h.", raw)
			continue
		}
		*d.target = rolling.Duration(parsed)
	}
	next.CooldownReset = form.Get("cooldown_reset") == "on"
	if len(errs) > 0 {
		return next, values, errs
	}
	if err := next.Validate(); err != nil {
		var fieldErr *rolling.FieldError
		if errors.As(err, &fieldErr) {
			errs[fieldErr.Field] = fieldErr.Message + "."
		} else {
			errs["prompt"] = err.Error()
		}
	}
	return next, values, errs
}

func handleSettings(req managementRequest) managementResponse {
	current, _ := keeper.currentSettings()
	next, values, errs := applySettingsForm(formValues(req), current)
	if len(errs) > 0 {
		view := buildSettings(values, next.CooldownReset, errs)
		view.Notice, view.NoticeErr = "Not saved. Fix the marked field.", true
		return render(http.StatusUnprocessableEntity, "settings-fragment", view)
	}
	patch := map[string]any{}
	was, now := settingsValues(current), settingsValues(next)
	for _, f := range settingsLabels {
		if was[f.name] != now[f.name] {
			patch[f.name] = now[f.name]
		}
	}
	if next.CooldownReset != current.CooldownReset {
		patch["cooldown_reset"] = next.CooldownReset
	}
	view := buildSettings(now, next.CooldownReset, nil)
	if len(patch) == 0 {
		view.Notice = "No changes."
		return render(http.StatusOK, "settings-fragment", view)
	}
	write, err := newWrite(patch)
	if err != nil {
		return render(http.StatusInternalServerError, "settings-fragment", view)
	}
	view.Write = write
	view.Notice = "Sent to the host config. The plugin applies it when the host reloads."
	return render(http.StatusOK, "settings-fragment", view)
}

// ---- Providers fragment ----

type providerRow struct {
	ID, Name, Mode, Model, DefaultModel, Note string
	Enabled, CanPing, SelectAll               bool
}

type providersView struct {
	URL       string
	Rows      []providerRow
	Notice    string
	NoticeErr bool
	Write     *writeView
}

func buildProviders(settings rolling.Settings, now time.Time) providersView {
	view := providersView{URL: pageToManagement + "/provider"}
	for _, info := range providerViews(settings, now) {
		p := settings.Provider(info.ID)
		row := providerRow{
			ID: info.ID, Name: info.Name, Model: p.Model, DefaultModel: info.DefaultModel, Note: info.Note,
			Enabled: p.Enabled, CanPing: info.CanPing, SelectAll: p.SelectAll,
		}
		switch {
		case info.PingInterval != "":
			row.Mode = "pinged every " + info.PingInterval
		case info.StartsOnFirstUse:
			row.Mode = "window starts on first use"
		default:
			row.Mode = "window not started by pings"
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

func validModel(model string) bool {
	if len(model) > 200 {
		return false
	}
	for _, r := range model {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// handleProvider changes one provider field: field=enabled|model|select_all.
func handleProvider(req managementRequest) managementResponse {
	form := formValues(req)
	current, _ := keeper.currentSettings()
	now := time.Now()
	fail := func(message string) managementResponse {
		view := buildProviders(current, now)
		view.Notice, view.NoticeErr = message, true
		return render(http.StatusUnprocessableEntity, "providers-fragment", view)
	}
	id := form.Get("provider")
	adapter, ok := provider.Get(id)
	if !ok {
		return fail("Unknown provider.")
	}
	next := cloneSettings(current)
	p := next.Provider(id)
	switch form.Get("field") {
	case "enabled":
		p.Enabled = form.Get("enabled") == "on"
	case "model":
		if !provider.CanPing(adapter) {
			return fail(adapter.Name() + " has no ping, so it has no model.")
		}
		model := strings.TrimSpace(form.Get("model"))
		if model == "" {
			model = adapter.DefaultModel()
		}
		if !validModel(model) {
			return fail("A model name has no spaces and at most 200 characters.")
		}
		p.Model = model
	case "select_all":
		p.SelectAll = form.Get("select_all") == "on"
		p.Accounts = []string{}
	default:
		return fail("Unknown provider field.")
	}
	next.Providers[id] = p
	view := buildProviders(next, now)
	if sameProvider(current.Provider(id), p) {
		view.Notice = "No changes."
		return render(http.StatusOK, "providers-fragment", view)
	}
	write, err := newWrite(providersPatch(next))
	if err != nil {
		return render(http.StatusInternalServerError, "providers-fragment", view)
	}
	view.Write = write
	view.Notice = "Sent to the host config. The plugin applies it when the host reloads."
	return render(http.StatusOK, "providers-fragment", view)
}

func sameProvider(a, b rolling.ProviderSettings) bool {
	return a.Enabled == b.Enabled && a.Model == b.Model && a.SelectAll == b.SelectAll && slices.Equal(a.Accounts, b.Accounts)
}

// handleSelect turns keep-rolling on or off for one account and returns the
// accounts fragment drawn with the new selection.
func handleSelect(req managementRequest) managementResponse {
	form := formValues(req)
	spec := parseSort(form)
	now := time.Now()
	current, configError := keeper.currentSettings()
	fail := func(status int, message string) managementResponse {
		return renderAccounts(status, buildStateWith(now, current, configError), spec, message, true, nil)
	}
	id := form.Get("account")
	accounts, err := accountSource()
	if err != nil {
		return fail(http.StatusOK, "")
	}
	idx := slices.IndexFunc(accounts, func(a tracked) bool { return a.entry.ID == id })
	if id == "" || idx < 0 {
		return fail(http.StatusUnprocessableEntity, "Account not found.")
	}
	account := accounts[idx]
	if account.entry.Disabled {
		return fail(http.StatusConflict, "The account is disabled in CLIProxyAPI.")
	}
	on := form.Get("on") == "1"
	providerID := account.adapter.ID()
	next := cloneSettings(current)
	p := next.Provider(providerID)
	if p.Selected(id) == on {
		return renderAccounts(http.StatusOK, buildStateWith(now, current, configError), spec, "", false, nil)
	}
	if p.SelectAll {
		// Turning one account off ends select_all: keep every other account of the provider.
		p.Accounts = []string{}
		for _, other := range accounts {
			if other.adapter.ID() == providerID && other.entry.ID != id {
				p.Accounts = append(p.Accounts, other.entry.ID)
			}
		}
	} else {
		p.Accounts = slices.DeleteFunc(p.Accounts, func(x string) bool { return x == id })
		if on {
			p.Accounts = append(p.Accounts, id)
		}
	}
	p.SelectAll = false
	next.Providers[providerID] = p
	write, err := newWrite(providersPatch(next))
	if err != nil {
		return fail(http.StatusInternalServerError, err.Error())
	}
	return renderAccounts(http.StatusOK, buildStateWith(now, next, configError), spec, "", false, write)
}

// ---- Whole authenticated view ----

type appView struct {
	Accounts  accountsView
	Providers providersView
	Settings  settingsView
	Status    statusView
}

func handleView(req managementRequest) managementResponse {
	now := time.Now()
	state := buildState(now)
	return render(http.StatusOK, "view", appView{
		Accounts:  buildAccounts(state, parseSort(req.Query)),
		Providers: buildProviders(state.Settings, now),
		Settings:  buildSettings(settingsValues(state.Settings), state.Settings.CooldownReset, nil),
		Status:    statusOf(state),
	})
}
