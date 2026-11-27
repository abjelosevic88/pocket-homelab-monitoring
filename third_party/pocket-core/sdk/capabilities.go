package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Capabilities (docs/capabilities.md). An app says in its manifest what it can
// do for the apps that bring everything together, and answers on one fixed
// route per capability, in one shape. Pocket Search, Pocket Day and Pocket Hub
// then find the app on their own: nobody adds it to a list in three other
// repositories.
//
//	capability   route                           scope            asked by
//	Search       GET  /api/v1/pocket/search      pocket:search    Pocket Search
//	Day          GET  /api/v1/pocket/day/{date}  pocket:day       Pocket Day
//	Widget       GET  /api/v1/pocket/widget      pocket:widget    Pocket Hub
//	Capture      POST /api/v1/pocket/capture     pocket:capture   Pocket Hub
//
// Register adds the offers on its own, like Portable's. The app mounts the
// handlers behind its normal auth (people may call them too) and calls
// Capabilities.LinkAllows from its linkAllows, so a link token with one of
// these scopes reaches exactly that route. Everything stays per person: the
// route runs as the person the caller acts for.
//
// The app that asks declares UseEvery(scope, reason) instead of one Use per
// app; Core then keeps a link to every app that offers the scope, new apps
// included, under one switch for the admin and one per app for each person.

const (
	ScopeSearch  = "pocket:search"
	ScopeDay     = "pocket:day"
	ScopeWidget  = "pocket:widget"
	ScopeCapture = "pocket:capture"

	SearchPath  = "/api/v1/pocket/search"
	DayPath     = "/api/v1/pocket/day/" // + YYYY-MM-DD
	WidgetPath  = "/api/v1/pocket/widget"
	CapturePath = "/api/v1/pocket/capture"

	// EveryApp as Use.App asks for the scopes in every app that offers them.
	EveryApp = "*"
)

// UseEvery asks for one scope in every app that offers it, apps installed
// later included. Core shows it as one switch plus a link per app.
func UseEvery(scope, reason string) Use {
	return Use{App: EveryApp, Scopes: []string{scope}, Reason: reason}
}

// Capabilities is what an app lets the box-wide apps do with it. It is part
// of the Manifest; keep one value and use it for the manifest, the routes and
// linkAllows.
type Capabilities struct {
	// Search: the person's things here can be found by words.
	Search *Searchable `json:"search,omitempty"`
	// Day: the app can say what happened here on a day.
	Day *DayFeed `json:"day,omitempty"`
	// Widget: a few lines for a home screen.
	Widget *Widget `json:"widget,omitempty"`
	// Capture: a quick line typed elsewhere can be kept here.
	Capture *QuickCapture `json:"capture,omitempty"`
}

// Searchable describes what Search finds in the app.
type Searchable struct {
	// What says it in plain words: "Your notes, by title and text".
	What string `json:"what,omitempty"`
	// Kinds an item can be, singular: "note", "task".
	Kinds []string `json:"kinds,omitempty"`
	// Bangs are short words that search only this app ("notes" → !notes).
	Bangs []string `json:"bangs,omitempty"`
}

// DayFeed describes the app's line in a day's journal.
type DayFeed struct {
	// Label heads the app's part of the day: "Notes". Default: the app's name without "Pocket ".
	Label string `json:"label,omitempty"`
	// One and Many name an item: "note", "notes".
	One  string `json:"one,omitempty"`
	Many string `json:"many,omitempty"`
	// Unit is what the day's number counts: count (default), min, amount or qty.
	Unit string `json:"unit,omitempty"`
	// What says in plain words what comes back: "The notes you wrote or changed that day".
	What string `json:"what,omitempty"`
}

// Widget describes the app's home-screen widget.
type Widget struct {
	// Title of the widget: "Tasks today".
	Title string `json:"title,omitempty"`
	// What says what it shows: "What is due today and what is late".
	What string `json:"what,omitempty"`
}

// QuickCapture lists what a quick line can become in the app.
type QuickCapture struct {
	Kinds []CaptureKind `json:"kinds,omitempty"`
}

// CaptureKind is one thing a quick line can become.
type CaptureKind struct {
	// Kind is the app's own word, sent back on capture: "note", "task".
	Kind string `json:"kind"`
	// Label for the button: "Note".
	Label string `json:"label,omitempty"`
	// What says where it lands: "A new note in Quick Notes".
	What string `json:"what,omitempty"`
	// Fields the kind takes besides text: "title", "due" (YYYY-MM-DD), "date" (YYYY-MM-DD).
	Fields []string `json:"fields,omitempty"`
}

// Any reports whether the app declared a capability.
func (c Capabilities) Any() bool {
	return c.Search != nil || c.Day != nil || c.Widget != nil || (c.Capture != nil && len(c.Capture.Kinds) > 0)
}

// Offers are the link scopes the capabilities need; Register adds them.
func (c Capabilities) Offers() []Offer {
	var out []Offer
	if c.Search != nil {
		out = append(out, Offer{Scope: ScopeSearch, Name: "Find your things", Description: "Search what you keep here by words and show the matches (Pocket Search). Only your own things; nothing is changed."})
	}
	if c.Day != nil {
		out = append(out, Offer{Scope: ScopeDay, Name: "See what happened on a day", Description: "Read what you did here on one day, for your journal (Pocket Day). Only your own things; nothing is changed."})
	}
	if c.Widget != nil {
		out = append(out, Offer{Scope: ScopeWidget, Name: "Show a summary", Description: "Read a few lines about where things stand, for a home screen (Pocket Hub). Only your own things; nothing is changed."})
	}
	if c.Capture != nil && len(c.Capture.Kinds) > 0 {
		out = append(out, Offer{Scope: ScopeCapture, Name: "Keep a quick line", Description: "Add something you typed elsewhere as a new item here (Pocket Hub). It adds; it never changes or deletes what you have."})
	}
	return out
}

// For returns the description behind a scope as JSON (nil when not declared).
func (c Capabilities) For(scope string) any {
	switch {
	case scope == ScopeSearch && c.Search != nil:
		return c.Search
	case scope == ScopeDay && c.Day != nil:
		return c.Day
	case scope == ScopeWidget && c.Widget != nil:
		return c.Widget
	case scope == ScopeCapture && c.Capture != nil && len(c.Capture.Kinds) > 0:
		return c.Capture
	}
	return nil
}

var dayPathRe = regexp.MustCompile(`^` + regexp.QuoteMeta(DayPath) + `\d{4}-\d{2}-\d{2}$`)

// LinkAllows reports whether a link token with these scopes may make this
// request under the capabilities: each scope opens its one route, with its
// one method, and only when the app declared the capability.
func (c Capabilities) LinkAllows(scopes []string, r *http.Request) bool {
	if r == nil {
		return false
	}
	has := func(want string) bool {
		for _, s := range scopes {
			if s == want {
				return true
			}
		}
		return false
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	path := r.URL.Path
	switch {
	case path == SearchPath:
		return read && c.Search != nil && has(ScopeSearch)
	case dayPathRe.MatchString(path):
		return read && c.Day != nil && has(ScopeDay)
	case path == WidgetPath:
		return read && c.Widget != nil && has(ScopeWidget)
	case path == CapturePath:
		return r.Method == http.MethodPost && c.Capture != nil && len(c.Capture.Kinds) > 0 && has(ScopeCapture)
	}
	return false
}

// addCapabilityOffers appends the pocket:* offers the manifest does not have yet.
func addCapabilityOffers(m *Manifest) {
	for _, o := range m.Capabilities.Offers() {
		dup := false
		for _, have := range m.Offers {
			if strings.EqualFold(have.Scope, o.Scope) {
				dup = true
			}
		}
		if !dup {
			m.Offers = append(m.Offers, o)
		}
	}
}

// ---- Search ----

// SearchItem is one match.
type SearchItem struct {
	// ID is the item's id in the app, as text.
	ID string `json:"id"`
	// Title is the line shown; an item without one is dropped.
	Title string `json:"title"`
	// Snippet is a line of context (plain text, no markup).
	Snippet string `json:"snippet,omitempty"`
	// Kind is what it is, singular: "note".
	Kind string `json:"kind,omitempty"`
	// Date the item is about or was last changed: RFC 3339 or YYYY-MM-DD.
	Date string `json:"date,omitempty"`
	// URL opens the item in the app: a path ("/note/12") or an absolute URL.
	URL string `json:"url,omitempty"`
	// Link is an address outside the box the item points at (a saved article).
	Link string `json:"link,omitempty"`
	// Score orders matches inside the app, higher first; 0 = the order given.
	Score float64 `json:"score,omitempty"`
}

// SearchResults is the answer of SearchPath.
type SearchResults struct {
	Items []SearchItem `json:"items"`
}

const (
	searchDefaultLimit = 8
	searchMaxLimit     = 50
	searchMaxQuery     = 200
)

// SearchHandler serves SearchPath (?q=&limit=) from the app's own search. fn
// gets the words (trimmed, at most 200 characters, never empty) and how many
// items are wanted (1–50, 8 when not said), and runs as the request's person.
// Mount it behind the app's normal auth.
func SearchHandler(fn func(r *http.Request, q string, limit int) ([]SearchItem, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if n := []rune(q); len(n) > searchMaxQuery {
			q = string(n[:searchMaxQuery])
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = searchDefaultLimit
		}
		if limit > searchMaxLimit {
			limit = searchMaxLimit
		}
		out := SearchResults{Items: []SearchItem{}}
		if q != "" {
			items, err := fn(r, q, limit)
			if err != nil {
				capabilityError(w, err)
				return
			}
			for _, it := range items {
				it.Title = oneLine(it.Title, 300)
				if it.Title == "" {
					continue
				}
				it.Snippet = oneLine(it.Snippet, 300)
				out.Items = append(out.Items, it)
				if len(out.Items) == limit {
					break
				}
			}
		}
		capabilityJSON(w, http.StatusOK, out)
	}
}

// ---- Day ----

// Day is what happened in the app on one day (the answer of DayPath + date).
type Day struct {
	Date string `json:"date"`
	// Summary is the day in one line: "3 notes written". Empty with no items = nothing happened.
	Summary string `json:"summary,omitempty"`
	// Items are the things of the day, in any order (the reader sorts by Time).
	Items []DayItem `json:"items"`
	// Metrics are the day's numbers worth showing next to the line.
	Metrics []Metric `json:"metrics,omitempty"`
}

// DayItem is one thing that happened, or is planned, on the day.
type DayItem struct {
	ID string `json:"id,omitempty"`
	// Time and End are local wall-clock "HH:MM"; empty = sometime that day.
	Time string `json:"time,omitempty"`
	End  string `json:"end,omitempty"`
	// Title is the line shown; an item without one is dropped.
	Title string `json:"title"`
	// Sub is a second, quieter line.
	Sub string `json:"sub,omitempty"`
	// Kind groups items of the day: "written", "edited".
	Kind string `json:"kind,omitempty"`
	// Minutes the thing took, when time is what counts.
	Minutes float64 `json:"minutes,omitempty"`
	// Done marks something finished (a task ticked off).
	Done bool `json:"done,omitempty"`
	// Amount is money in minor units of Currency (450 = 4.50).
	Amount   int64  `json:"amount,omitempty"`
	Currency string `json:"currency,omitempty"`
	// Qty is a number of something else (pages, kilometres).
	Qty float64 `json:"qty,omitempty"`
	// URL opens the item in the app: a path or an absolute URL.
	URL string `json:"url,omitempty"`
	// Image is a small picture: a path in the app or an absolute URL.
	Image string `json:"image,omitempty"`
	// Note is the person's own words about it.
	Note string `json:"note,omitempty"`
}

// Metric is one labelled number: {"Done", "4 of 6"}.
type Metric struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// DayHandler serves DayPath + date. fn gets a valid YYYY-MM-DD and the
// person's time zone when the caller said one (?tz=, else nil: use the app's
// own), and runs as the request's person. Return nil for a day with nothing.
// Mount it behind the app's normal auth, on the path with the date in it.
func DayHandler(fn func(r *http.Request, date string, tz *time.Location) (*Day, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		date := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if _, err := time.Parse("2006-01-02", date); err != nil || !dateRe.MatchString(date) {
			capabilityJSON(w, http.StatusBadRequest, map[string]string{"error": "the day must be YYYY-MM-DD"})
			return
		}
		var tz *time.Location
		if name := r.URL.Query().Get("tz"); name != "" {
			if loc, err := time.LoadLocation(name); err == nil {
				tz = loc
			}
		}
		day, err := fn(r, date, tz)
		if err != nil {
			capabilityError(w, err)
			return
		}
		out := Day{Date: date, Items: []DayItem{}}
		if day != nil {
			out.Summary, out.Metrics = oneLine(day.Summary, 300), day.Metrics
			for _, it := range day.Items {
				it.Title = oneLine(it.Title, 300)
				if it.Title == "" {
					continue
				}
				it.Sub = oneLine(it.Sub, 300)
				out.Items = append(out.Items, it)
				if len(out.Items) == 200 {
					break
				}
			}
		}
		capabilityJSON(w, http.StatusOK, out)
	}
}

// ---- Widget ----

// WidgetData is what a home screen shows for the app (the answer of WidgetPath).
type WidgetData struct {
	// Line says where things stand: "2 due today, 1 late". Empty = nothing to say.
	Line string `json:"line,omitempty"`
	// Metrics are up to four numbers.
	Metrics []Metric `json:"metrics,omitempty"`
	// Items are the few things worth a glance, most important first.
	Items []WidgetItem `json:"items"`
	// URL opens the place in the app the widget is about; default the app itself.
	URL string `json:"url,omitempty"`
}

// WidgetItem is one row of a widget.
type WidgetItem struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Done   bool   `json:"done,omitempty"`
	URL    string `json:"url,omitempty"`
}

// WidgetHandler serves WidgetPath. fn runs as the request's person; nil = nothing to show.
func WidgetHandler(fn func(r *http.Request) (*WidgetData, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := fn(r)
		if err != nil {
			capabilityError(w, err)
			return
		}
		out := WidgetData{Items: []WidgetItem{}}
		if d != nil {
			out.Line, out.URL, out.Metrics = oneLine(d.Line, 200), d.URL, d.Metrics
			if len(out.Metrics) > 4 {
				out.Metrics = out.Metrics[:4]
			}
			for _, it := range d.Items {
				it.Title = oneLine(it.Title, 200)
				if it.Title == "" {
					continue
				}
				it.Detail = oneLine(it.Detail, 200)
				out.Items = append(out.Items, it)
				if len(out.Items) == 8 {
					break
				}
			}
		}
		capabilityJSON(w, http.StatusOK, out)
	}
}

// ---- Capture ----

// CaptureInput is a quick line to keep (the body of CapturePath).
type CaptureInput struct {
	// RequestID names the attempt (16–80 of A-Z a-z 0-9 _ -): the same id again must not add a second item.
	RequestID string `json:"request_id"`
	// Kind is one of the app's CaptureKinds; empty = the first.
	Kind  string `json:"kind,omitempty"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text,omitempty"`
	// Due and Date are YYYY-MM-DD.
	Due  string `json:"due,omitempty"`
	Date string `json:"date,omitempty"`
}

// CaptureResult is what was made.
type CaptureResult struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// URL opens it in the app: a path or an absolute URL.
	URL string `json:"url,omitempty"`
}

// ErrCapture is a capture the person can fix (an empty line, an unknown
// kind); CaptureHandler answers 400 with its text. Wrap it: fmt.Errorf("%w: …", ErrCapture).
var ErrCapture = errors.New("can't keep that")

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,80}$`)

// CaptureHandler serves CapturePath for the kinds the app declared. fn gets a
// checked input (a known kind, a request id, a title or a text; dates valid)
// and runs as the request's person; it makes one new item and must answer the
// same for the same RequestID (Client.NewOperation does that).
func (q *QuickCapture) CaptureHandler(fn func(r *http.Request, in CaptureInput) (*CaptureResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bad := func(msg string) { capabilityJSON(w, http.StatusBadRequest, map[string]string{"error": msg}) }
		if q == nil || len(q.Kinds) == 0 {
			capabilityJSON(w, http.StatusNotFound, map[string]string{"error": "nothing can be kept here"})
			return
		}
		var in CaptureInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&in); err != nil {
			bad("invalid json")
			return
		}
		in.Kind, in.Title, in.Text = strings.TrimSpace(in.Kind), strings.TrimSpace(in.Title), strings.TrimSpace(in.Text)
		if in.Kind == "" {
			in.Kind = q.Kinds[0].Kind
		}
		known := false
		for _, k := range q.Kinds {
			known = known || k.Kind == in.Kind
		}
		switch {
		case !known:
			bad("this app does not keep a " + in.Kind)
			return
		case !requestIDRe.MatchString(in.RequestID):
			bad("request_id must be 16–80 letters, digits, _ or -")
			return
		case in.Title == "" && in.Text == "":
			bad("write something first")
			return
		case len(in.Title) > 500 || len(in.Text) > 100_000:
			bad("that is too long")
			return
		}
		for _, d := range []string{in.Due, in.Date} {
			if d == "" {
				continue
			}
			if _, err := time.Parse("2006-01-02", d); err != nil {
				bad("dates must be YYYY-MM-DD")
				return
			}
		}
		res, err := fn(r, in)
		switch {
		case errors.Is(err, ErrCapture):
			bad(strings.TrimPrefix(err.Error(), ErrCapture.Error()+": "))
			return
		case err != nil:
			capabilityError(w, err)
			return
		case res == nil || res.ID == "":
			capabilityJSON(w, http.StatusInternalServerError, map[string]string{"error": "nothing was kept"})
			return
		}
		capabilityJSON(w, http.StatusCreated, res)
	}
}

func capabilityJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// capabilityError answers 500 without the error's text: it may name rows or paths.
func capabilityError(w http.ResponseWriter, _ error) {
	capabilityJSON(w, http.StatusInternalServerError, map[string]string{"error": "the app could not answer"})
}

// oneLine folds whitespace and cuts to max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return s
}

// ---- The asking side ----

// CapableApp is an app that offers a capability, as it stands for one person.
type CapableApp struct {
	// App is the slug; Name the name people know it by.
	App  string `json:"app"`
	Name string `json:"name"`
	// Icon is an absolute URL (may be empty); PublicURL is what a browser opens.
	Icon      string `json:"icon,omitempty"`
	URL       string `json:"url,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
	Version   string `json:"version,omitempty"`
	Category  string `json:"category,omitempty"`
	// On: the link works for the person now. State and Detail say why not.
	On     bool   `json:"on"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// What the app said about the capability in its manifest.
	Capabilities
}

// Capable lists the apps that offer a capability scope and how the link to
// each stands for the person (declare UseEvery(scope, …) in the manifest).
// Apps that are not registered or no longer offer it are left out.
func (c *Client) Capable(ctx context.Context, email, scope string) ([]CapableApp, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	var rows []struct {
		App    string `json:"app"`
		Scope  string `json:"scope"`
		State  string `json:"state"`
		On     bool   `json:"on"`
		Detail string `json:"detail"`
		Target *struct {
			Name      string `json:"name"`
			Icon      string `json:"icon"`
			URL       string `json:"url"`
			PublicURL string `json:"public_url"`
			Version   string `json:"version"`
			Category  string `json:"category"`
			Capabilities
		} `json:"target"`
	}
	q := url.Values{"user": {email}, "scope": {scope}}
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/links/status?"+q.Encode(), c.Token(), nil, &rows); err != nil {
		return nil, err
	}
	out := make([]CapableApp, 0, len(rows))
	for _, r := range rows {
		if r.Scope != scope || r.Target == nil || r.State == "target_missing" || r.State == "not_offered" {
			continue
		}
		out = append(out, CapableApp{App: r.App, Name: r.Target.Name, Icon: r.Target.Icon, URL: r.Target.URL, PublicURL: r.Target.PublicURL,
			Version: r.Target.Version, Category: r.Target.Category, On: r.On, State: r.State, Detail: r.Detail, Capabilities: r.Target.Capabilities})
	}
	return out, nil
}

// capabilityCall acts for the person in app with one scope and makes one
// request there; it returns the address a browser opens the app at.
func (c *Client) capabilityCall(ctx context.Context, email, app, scope, method, path string, body, out any) (string, error) {
	tok, err := c.ActAs(ctx, email, app, scope)
	if err != nil {
		return "", err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(tok.URL, "/")+path, rd)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s did not answer: %w", app, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = http.StatusText(res.StatusCode)
		}
		return "", &CapabilityError{App: app, Status: res.StatusCode, Message: e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return "", fmt.Errorf("%s answered something else than JSON: %w", app, err)
		}
	}
	public := tok.PublicURL
	if public == "" {
		public = tok.URL
	}
	return strings.TrimRight(public, "/"), nil
}

// CapabilityError is an app refusing or failing a capability call. A link
// that is off is a *LinkError instead (from ActAs).
type CapabilityError struct {
	App     string
	Status  int
	Message string
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d)", e.App, e.Message, e.Status)
}

// absURL makes an item's address something a browser can open.
func absURL(public, u string) string {
	switch {
	case u == "":
		return ""
	case strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://"):
		return u
	case strings.HasPrefix(u, "/"):
		return public + u
	}
	return public + "/" + u
}

// SearchIn searches one app for the person. Item URLs come back absolute.
func (c *Client) SearchIn(ctx context.Context, email, app, q string, limit int) ([]SearchItem, error) {
	var res SearchResults
	query := url.Values{"q": {q}}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	public, err := c.capabilityCall(ctx, email, app, ScopeSearch, http.MethodGet, SearchPath+"?"+query.Encode(), nil, &res)
	if err != nil {
		return nil, err
	}
	for i := range res.Items {
		res.Items[i].URL = absURL(public, res.Items[i].URL)
	}
	return res.Items, nil
}

// DayIn reads what happened in one app on a day (YYYY-MM-DD) for the person;
// tz may be nil. Item URLs and images come back absolute.
func (c *Client) DayIn(ctx context.Context, email, app, date string, tz *time.Location) (*Day, error) {
	path := DayPath + url.PathEscape(date)
	if tz != nil && tz.String() != "" && tz.String() != "Local" {
		path += "?" + url.Values{"tz": {tz.String()}}.Encode()
	}
	var day Day
	public, err := c.capabilityCall(ctx, email, app, ScopeDay, http.MethodGet, path, nil, &day)
	if err != nil {
		return nil, err
	}
	for i := range day.Items {
		day.Items[i].URL = absURL(public, day.Items[i].URL)
		day.Items[i].Image = absURL(public, day.Items[i].Image)
	}
	return &day, nil
}

// WidgetIn reads one app's widget for the person. URLs come back absolute.
func (c *Client) WidgetIn(ctx context.Context, email, app string) (*WidgetData, error) {
	var d WidgetData
	public, err := c.capabilityCall(ctx, email, app, ScopeWidget, http.MethodGet, WidgetPath, nil, &d)
	if err != nil {
		return nil, err
	}
	if d.URL = absURL(public, d.URL); d.URL == "" {
		d.URL = public
	}
	for i := range d.Items {
		d.Items[i].URL = absURL(public, d.Items[i].URL)
	}
	return &d, nil
}

// CaptureIn keeps a quick line in one app for the person. The URL comes back absolute.
func (c *Client) CaptureIn(ctx context.Context, email, app string, in CaptureInput) (*CaptureResult, error) {
	var res CaptureResult
	public, err := c.capabilityCall(ctx, email, app, ScopeCapture, http.MethodPost, CapturePath, in, &res)
	if err != nil {
		return nil, err
	}
	res.URL = absURL(public, res.URL)
	return &res, nil
}
