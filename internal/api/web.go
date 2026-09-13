package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// The web UI is served by the API process from a directory of built files.
//
// SAME ORIGIN, deliberately. The alternative -- a separate static server --
// makes the browser treat the UI and the API as different origins, which means
// CORS configuration, preflight requests on every non-simple call, and a
// credentials policy to get right. Serving both from one origin makes all of
// that unnecessary: there is no cross-origin request to permit.
//
// A directory on disk rather than go:embed, so that `go build` and `go test`
// never depend on a Node toolchain having run first. The Docker image copies
// the built files in; a developer running the API locally without a UI build
// simply gets no UI, and every API route still works.

// webSecurityHeaders are set on every UI response.
//
// The CSP is strict because it can be: the Vite build emits external module
// scripts and stylesheets and no inline code, so nothing needs 'unsafe-inline'.
//
//   - default-src 'self'      nothing loads from anywhere but this server --
//     no CDN, no analytics, no third party ever sees that this page exists.
//   - img-src 'self' data:    thumbnails come from the API; data: covers
//     small inline SVG icons.
//   - frame-ancestors 'none'  the page cannot be framed, which closes
//     clickjacking. X-Frame-Options repeats it for older browsers.
//   - base-uri / form-action  an injected <base> or <form> cannot redirect
//     relative URLs or submissions elsewhere.
//
// Referrer-Policy no-referrer: the UI's URLs contain library ids and search
// terms, and there is no reason for any of that to leave the machine.
func webSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
			"connect-src 'self'; object-src 'none'; base-uri 'none'; "+
			"form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// webHandler serves a single-page application from dir.
//
// Three kinds of request, and getting the boundaries between them right is the
// whole job:
//
//  1. A path naming a real file -- /assets/index-3f2a.js -- is that file.
//  2. A path with no file extension -- /libraries/abc/review -- is a client-
//     side route. The server knows nothing about it, so it returns index.html
//     and lets the app's router decide what to render.
//  3. A path WITH an extension that does not exist -- /assets/index-OLD.js --
//     is a 404. Returning index.html here is the classic SPA-fallback bug: the
//     browser receives HTML where it asked for JavaScript, and reports a MIME
//     type error that points nowhere near the actual cause, a stale cached
//     index.html referencing an asset from a previous build.
func webHandler(dir string) http.Handler {
	root := os.DirFS(dir)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// path.Clean on a rooted path cannot climb above the root: "/../x"
		// cleans to "/x". Trimming the leading slash then yields a name that
		// fs.FS itself validates -- os.DirFS rejects any name containing ".."
		// elements or a leading separator -- so traversal is refused twice.
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		webSecurityHeaders(w)

		if info, err := fs.Stat(root, name); err == nil && info.Mode().IsRegular() {
			setWebCacheHeaders(w, name)
			http.ServeFileFS(w, r, root, name)
			return
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid) {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Not a file. An extension means the client asked for an asset that
		// does not exist -- case 3 above.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}

		// A client-side route. index.html must never be cached: it is the one
		// file that names the current build's hashed assets, so a stale copy
		// keeps loading the previous build indefinitely.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, root, "index.html")
	})
}

// setWebCacheHeaders applies the caching policy for a static file.
//
// Vite content-hashes everything under assets/, so a given URL's bytes can
// never change -- a new build produces a new filename. Those are cached for a
// year and marked immutable, which stops the browser even revalidating them.
// Everything else, index.html above all, is revalidated on every load.
func setWebCacheHeaders(w http.ResponseWriter, name string) {
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}

// apiNotFound answers unmatched /api/ paths with a JSON 404.
//
// Registered on the /api/ prefix so that the SPA's catch-all never swallows an
// API request. Without it, a client calling a misspelled endpoint would receive
// index.html with a 200 status, and fail somewhere far away trying to parse
// HTML as JSON.
func (s *Server) apiNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, loggerFrom(r.Context(), s.log), http.StatusNotFound,
		"not_found", "no such API endpoint")
}
