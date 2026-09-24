package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const publicBase = "/gly"

type appBaseKey struct{}

func appBase(r *http.Request) string {
	base, _ := r.Context().Value(appBaseKey{}).(string)
	return base
}

func appHome(r *http.Request) string {
	if base := appBase(r); base != "" {
		return base
	}
	return "/"
}

func (s *server) mountRequest(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	host, err := url.Parse("http://" + r.Host)
	if err != nil || host.Host != r.Host || host.Hostname() == "" || host.User != nil || host.Path != "" || host.RawQuery != "" || host.Fragment != "" {
		http.NotFound(w, r)
		return r, false
	}
	path := r.URL.Path
	if localOrigin("http://" + r.Host) {
		if path == publicBase || strings.HasPrefix(path, publicBase+"/") {
			http.NotFound(w, r)
			return r, false
		}
		return r, true
	}
	if path != publicBase && !strings.HasPrefix(path, publicBase+"/") {
		http.NotFound(w, r)
		return r, false
	}
	if path == publicBase+"/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		http.Redirect(w, r, publicBase, http.StatusPermanentRedirect)
		return r, false
	}
	copy := r.Clone(context.WithValue(r.Context(), appBaseKey{}, publicBase))
	copy.URL.Path = strings.TrimPrefix(path, publicBase)
	if copy.URL.Path == "" {
		copy.URL.Path = "/"
	}
	copy.URL.RawPath = ""
	return copy, true
}

var publicFiles = map[string]bool{
	"base.css": true, "fields.css": true, "auth.css": true,
	"http.js": true, "password-input.js": true, "login.js": true,
	"assets/rykvo-voice.png": true,
}

func (s *server) serveWeb(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Vary", "Cookie")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(405)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name != "" && (!fs.ValidPath(name) || strings.Contains(name, "\\") || strings.HasPrefix(name, ".")) {
		http.NotFound(w, r)
		return
	}
	if !publicFiles[name] {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, err := s.readSession(ctx, r)
		signedIn := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Service unavailable", 503)
			return
		}
		switch name {
		case "":
			if signedIn {
				name = "index.html"
			} else {
				name = "login.html"
			}
		case "index.html", "login.html":
			http.Redirect(w, r, appHome(r), http.StatusSeeOther)
			return
		default:
			if !signedIn {
				http.Error(w, "Authentication required", 401)
				return
			}
			ext := filepath.Ext(name)
			if strings.Contains(name, "/") {
				if !strings.HasPrefix(name, "assets/") || (ext != ".png" && ext != ".svg") {
					http.NotFound(w, r)
					return
				}
			} else if ext != ".js" && ext != ".css" {
				http.NotFound(w, r)
				return
			}
		}
	}
	file, err := os.Open(filepath.Join(s.webRoot, filepath.FromSlash(name)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if appBase(r) != "" && (name == "index.html" || name == "login.html") {
		html, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "Service unavailable", 503)
			return
		}
		html = bytes.Replace(html, []byte("<head>"), []byte("<head><base href=\""+publicBase+"/\">"), 1)
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(html))
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}
