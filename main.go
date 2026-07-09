// Command godeploy is a self-contained GitOps-style continuous deployment
// controller for services running on systemd hosts.
//
// Usage:
//
//	godeploy -config /etc/godeploy/config.json
package main

import (
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"

	"godeploy/internal/api"
	"godeploy/internal/auth"
	"godeploy/internal/config"
	"godeploy/internal/deploy"
	"godeploy/internal/notify"
	"godeploy/internal/registry"
	"godeploy/internal/store"
	"godeploy/web"
)

func main() {
	configPath := flag.String("config", "config.json", "path to godeploy config file (JSON)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.Server.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	notifier := notify.New(cfg.Notifications)
	authMgr := auth.New(cfg.Auth, st)
	if err := authMgr.LoadBootstrapUsers(); err != nil {
		log.Fatalf("auth bootstrap: %v", err)
	}

	reg := registry.New(cfg, st)
	engine := deploy.New(reg, st, notifier)

	reconciler := deploy.NewReconciler(reg, engine, st)
	reconciler.Start()

	mux := http.NewServeMux()
	a := api.New(cfg, reg, st, engine, authMgr)
	a.Routes(mux)

	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		log.Fatalf("static assets: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	// Auth mode summary log
	if len(cfg.Auth.BootstrapUsers) > 0 {
		log.Printf("🔐 username/password auth enabled (%d bootstrap user(s))", len(cfg.Auth.BootstrapUsers))
	} else if cfg.Auth.ResolvedToken() != "" {
		log.Printf("🔑 bearer token auth enabled (set bootstrapUsers in config to enable login UI)")
	} else {
		log.Println("⚠️  no authentication configured — OPEN ACCESS (dev/loopback only)")
	}

	log.Printf("🚀 godeploy listening on %s (%d static app(s), data dir %s)",
		cfg.Server.ListenAddr, len(cfg.Apps), cfg.Server.DataDir)

	server := &http.Server{Addr: cfg.Server.ListenAddr, Handler: mux}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
}
