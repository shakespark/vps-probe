// Package web embeds the single-page UI. Files are loaded and gzipped once
// at startup and served from memory.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var files embed.FS

type asset struct {
	body, gz  []byte
	ctype     string
	etag      string
	immutable bool // versioned vendor files never change under the same name
}

// Handler serves / (index.html) and /static/*. It is meant to be mounted
// on "GET /{$}" and "GET /static/"; anything else is not found.
type Handler struct {
	assets map[string]*asset // URL path -> asset
}

func New() (*Handler, error) {
	h := &Handler{assets: map[string]*asset{}}
	err := fs.WalkDir(files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		a := &asset{
			body:      body,
			ctype:     mime.TypeByExtension(path.Ext(p)),
			etag:      `"` + hex.EncodeToString(sum[:8]) + `"`,
			immutable: strings.HasPrefix(p, "static/vendor/"),
		}
		if a.ctype == "" {
			a.ctype = "application/octet-stream"
		}
		if len(body) > 1024 {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(body)
			zw.Close()
			a.gz = buf.Bytes()
		}
		url := "/" + p
		if p == "static/index.html" {
			url = "/"
		}
		h.assets[url] = a
		return nil
	})
	return h, err
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a, ok := h.assets[r.URL.Path]
	if !ok || r.URL.Path == "/static/index.html" {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", a.ctype)
	hd.Set("ETag", a.etag)
	hd.Add("Vary", "Accept-Encoding")
	if a.immutable {
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		hd.Set("Cache-Control", "no-cache") // revalidate; unchanged files get 304
	}
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.body
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		hd.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}
