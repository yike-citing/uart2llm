package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var files embed.FS

func Handler() http.Handler {
	root, _ := fs.Sub(files, "assets")
	f := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		f.ServeHTTP(w, r)
	})
}
