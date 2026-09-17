package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func serveStatic(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := dashboard()
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestServesThePage(t *testing.T) {
	w := serveStatic(t, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<title>bsc — dashboard</title>") {
		t.Fatal("index.html is not what came back")
	}
}

// Everything is embedded, so there is still one artefact to ship. A missing
// asset here means the binary serves a broken page rather than failing to
// build, which is exactly the kind of thing that survives to production.
func TestEveryAssetThePageReferencesIsEmbedded(t *testing.T) {
	page := serveStatic(t, "/").Body.String() // FileServer redirects /index.html to /
	for _, asset := range []string{"style.css", "app.js"} {
		if !strings.Contains(page, asset) {
			t.Errorf("index.html no longer references %s — update this test or the page", asset)
		}
		if w := serveStatic(t, "/"+asset); w.Code != http.StatusOK {
			t.Errorf("%s: status %d, want it embedded and served", asset, w.Code)
		}
	}
}

// The page reflects live state and ships inside the binary, so a cached copy is
// only ever a stale dashboard after an upgrade — and a stale dashboard for a
// payments service is worse than no dashboard.
func TestPageIsNeverCached(t *testing.T) {
	if got := serveStatic(t, "/").Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// No CDN, no build step, no imports: the page must render on a box with no
// internet, which is where this service runs. The only external URLs allowed
// are block-explorer links, which are somewhere an operator clicks *to* rather
// than something the page loads.
func TestPageLoadsNothingExternal(t *testing.T) {
	external := regexp.MustCompile(`https?://[^\s"'` + "`" + `)]+`)
	for _, path := range []string{"/", "/style.css", "/app.js"} {
		for _, url := range external.FindAllString(serveStatic(t, path).Body.String(), -1) {
			if strings.Contains(url, "bscscan.com") {
				continue
			}
			t.Errorf("%s references %s — the page must be self-contained", path, url)
		}
	}
}
