package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// The dashboard's static files: a plain HTML/CSS/JS page for watching the
// service work and driving it by hand.
//
// It is disabled by default and, when on, exists only on the one listener —
// which is loopback-only and unauthenticated by design. It is a sandbox and an
// operator's window, not a product surface: there is no auth, no notion of who
// is looking, and anyone who can load it can move every wallet's money (§29).
//
// The page is deliberately plain. No build step, no framework, no CDN: the
// service is one binary with one file on disk, and a dashboard that needed npm
// to change would not survive contact with that. Everything here is embedded,
// so there is still exactly one artefact to ship.

//go:embed static
var static embed.FS

// dashboard serves the static files. Mount it under a prefix with
// http.StripPrefix.
func dashboard() (http.Handler, error) {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The page reflects live state and is rebuilt with the binary; caching
		// it only ever serves a stale dashboard after an upgrade.
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	}), nil
}
