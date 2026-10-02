package main

import (
	"net/http"
	"runtime/debug"
)

var buildVersion = "development"

func versionAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "METHOD_NOT_ALLOWED")
		return
	}
	revision := ""
	dirty := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, v := range info.Settings {
			switch v.Key {
			case "vcs.revision":
				revision = v.Value
			case "vcs.modified":
				dirty = v.Value == "true"
			}
		}
	}
	reply(w, 200, map[string]any{"data": map[string]any{"version": buildVersion, "revision": revision, "modified": dirty}})
}
