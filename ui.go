package main

import (
	"embed"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

type uiState struct {
	status
	Autostart bool `json:"autostart"`
	CanFix    bool `json:"canFix"`
}

// serveUI serves the window's page and a small JSON API, only to this
// computer and only with the secret token embedded in the page.
func serveUI(ln net.Listener, app *App) {
	token := app.cfg.UIToken
	page, _ := webFS.ReadFile("web/index.html")
	page = []byte(strings.Replace(string(page), "__TOKEN__", token, 1))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.URL.Query().Get("t") != token {
			http.Error(w, "Apri Ponte dal suo collegamento.", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	api := func(path string, h func(body map[string]string) any) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Ponte-Token") != token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			body := map[string]string{}
			if r.Method == http.MethodPost {
				json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(h(body))
		})
	}
	state := func() any {
		s := uiState{status: app.status(), Autostart: autostartEnabled()}
		s.CanFix = s.ErrorHelp != "" && canFixPermissions()
		return s
	}
	api("GET /api/state", func(map[string]string) any { return state() })
	api("POST /api/role", func(b map[string]string) any { app.setRole(b["role"]); return state() })
	api("POST /api/edge", func(b map[string]string) any { app.setEdge(b["edge"]); return state() })
	api("POST /api/name", func(b map[string]string) any { app.setName(strings.TrimSpace(b["name"])); return state() })
	api("POST /api/connect", func(b map[string]string) any {
		app.connect(b["id"], b["addr"], b["code"])
		return state()
	})
	api("POST /api/clipboard", func(b map[string]string) any { app.setClipboard(b["on"] == "1"); return state() })
	api("POST /api/forget", func(b map[string]string) any { app.forget(b["id"]); return state() })
	api("POST /api/autostart", func(b map[string]string) any {
		if err := setAutostart(b["on"] == "1"); err != nil {
			logf("avvio automatico: %v", err)
		}
		return state()
	})
	api("POST /api/fix", func(map[string]string) any {
		if err := fixPermissions(); err != nil {
			return map[string]string{"error": err.Error()}
		}
		app.mu.Lock()
		role := app.cfg.Role
		app.mu.Unlock()
		app.setRole(role)
		return map[string]string{"ok": "1"}
	})
	api("POST /api/show", func(map[string]string) any { showWindow(); return map[string]string{"ok": "1"} })
	api("POST /api/quit", func(map[string]string) any {
		go func() {
			time.Sleep(200 * time.Millisecond)
			quitApp(app)
		}()
		return map[string]string{"ok": "1"}
	})

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Refuse requests that reach us through another host name
			// (DNS rebinding from a web page).
			host, _, _ := net.SplitHostPort(r.Host)
			if host != "127.0.0.1" && host != "localhost" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	srv.Serve(ln)
}
