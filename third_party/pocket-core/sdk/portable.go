package sdk

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Moving in and out (docs/move.md). An app that sets Manifest.Portable lets
// Pocket Move copy a person's data out of it (the files in Exports) and bring
// data in (the Imports), through ordinary app links:
//
//   - Exports → scope move:export (GET only, the listed paths)
//   - Imports → scope move:import (POST only, the listed paths)
//
// Register adds both offers on its own, like events:read. The app answers
// GET PortablePath with its Portable (mount Portable.Handler there, behind the
// normal auth) and calls Portable.LinkAllows from its linkAllows, so a link
// token with these scopes reaches exactly these routes and nothing else.
// Everything stays per person: the routes run as the person the link acts for.

const (
	ScopeMoveExport = "move:export"
	ScopeMoveImport = "move:import"
	// PortablePath is where an app describes what can be moved.
	PortablePath = "/api/v1/pocket/portable"
)

// Open-format kinds Pocket Move understands. KindPocket is an app's own full
// export: it is what Move feeds back into the same app on another box.
const (
	KindPocket      = "pocket"          // the app's own export (JSON or zip), re-importable into the same app
	KindVCard       = "vcard"           // text/vcard, one or more VCARDs
	KindICalEvents  = "ical-events"     // text/calendar with VEVENTs
	KindICalTodos   = "ical-todos"      // text/calendar with VTODOs
	KindMarkdownZip = "markdown-zip"    // zip of .md files (folders = folders), attachments next to them
	KindOPMLFeeds   = "opml-feeds"      // OPML of RSS/Atom feeds
	KindOPMLPodcast = "opml-podcasts"   // OPML of podcast feeds
	KindGoodreads   = "goodreads-csv"   // Goodreads library export
	KindBankCSV     = "bank-csv"        // a bank or card statement as CSV
	KindPlaces      = "geojson-places"  // GeoJSON FeatureCollection of saved places (Google Maps "Saved Places")
	KindTimeline    = "google-timeline" // Google location history (Records.json, Timeline.json or Semantic Location History)
	KindCSV         = "csv"             // a generic table
	KindJSON        = "json"            // generic JSON
	KindZip         = "zip"             // anything else zipped
	KindMarkdown    = "markdown"        // one Markdown file
	KindGPX         = "gpx"             // GPS tracks and routes
)

// Portable is what an app lets Pocket Move take out and bring in.
type Portable struct {
	Exports []PortableExport `json:"exports,omitempty"`
	Imports []PortableImport `json:"imports,omitempty"`
}

// PortableExport is one file a person can take with them.
type PortableExport struct {
	// Path is the GET route, e.g. "/api/v1/export.json".
	Path string `json:"path"`
	// File is the name inside the moving-out archive, e.g. "notes.zip".
	File string `json:"file"`
	// Kind is one of the Kind* constants; KindPocket for the app's own full export.
	Kind string `json:"kind"`
	// Title says in plain words what is in it ("Every note as a Markdown file").
	Title string `json:"title,omitempty"`
}

// PortableImport is one way in.
type PortableImport struct {
	// Path is the POST route, e.g. "/api/v1/import".
	Path string `json:"path"`
	// Kind is the format the route takes (Kind* constants).
	Kind string `json:"kind"`
	// Form is the multipart field the file goes in; empty = the raw file as the body.
	Form string `json:"form,omitempty"`
	// ContentType for a raw body ("application/json", "text/vcard"…); empty = application/octet-stream.
	ContentType string `json:"content_type,omitempty"`
	// MaxBytes is the largest file the route takes; 0 = the app's usual body limit.
	MaxBytes int64 `json:"max_bytes,omitempty"`
	// Title says what happens ("Adds the notes; existing notes are left alone").
	Title string `json:"title,omitempty"`
}

// Offers are the link scopes Portable needs; Register adds them.
func (p *Portable) Offers() []Offer {
	if p == nil {
		return nil
	}
	var out []Offer
	if len(p.Exports) > 0 {
		out = append(out, Offer{Scope: ScopeMoveExport, Name: "Copy your data out", Description: "Read everything you keep here as files, to take it to another box or keep a copy (Pocket Move). Only your own data."})
	}
	if len(p.Imports) > 0 {
		out = append(out, Offer{Scope: ScopeMoveImport, Name: "Bring your data in", Description: "Add data you bring from another app or another box (Pocket Move). It adds; it never deletes what you have."})
	}
	return out
}

// LinkAllows reports whether a link token with these scopes may make this
// request under Portable: PortablePath with either scope, an export path
// (GET) with move:export, an import path (POST) with move:import.
func (p *Portable) LinkAllows(scopes []string, r *http.Request) bool {
	if p == nil || r == nil {
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
	path := r.URL.Path
	if path == PortablePath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		return (has(ScopeMoveExport) && len(p.Exports) > 0) || (has(ScopeMoveImport) && len(p.Imports) > 0)
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && has(ScopeMoveExport) {
		for _, e := range p.Exports {
			if e.Path == path {
				return true
			}
		}
	}
	if r.Method == http.MethodPost && has(ScopeMoveImport) {
		for _, i := range p.Imports {
			if i.Path == path {
				return true
			}
		}
	}
	return false
}

// Handler serves the description at PortablePath. Mount it behind the app's
// normal auth (people may read it too).
func (p *Portable) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := Portable{}
		if p != nil {
			out = *p
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// addPortableOffers appends the move:* offers the manifest does not have yet.
func addPortableOffers(m *Manifest) {
	for _, o := range m.Portable.Offers() {
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
