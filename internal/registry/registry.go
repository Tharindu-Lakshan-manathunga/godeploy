package registry

import (
	"fmt"

	"godeploy/internal/config"
	"godeploy/internal/store"
)

type Registry struct {
	cfg *config.Config
	st  *store.Store
}

func New(cfg *config.Config, st *store.Store) *Registry {
	return &Registry{cfg: cfg, st: st}
}

func (r *Registry) AllApps() ([]config.App, error) {
	apps := make([]config.App, 0, len(r.cfg.Apps))
	for _, app := range r.cfg.Apps {
		apps = append(apps, app)
	}
	dyn := r.st.ListDynamicApps()
	apps = append(apps, dyn...)
	return apps, nil
}

func (r *Registry) FindApp(name string) (config.App, bool) {
	if app, ok := r.getStaticApp(name); ok {
		return app, true
	}
	return r.st.GetDynamicApp(name)
}

func (r *Registry) getStaticApp(name string) (config.App, bool) {
	for _, app := range r.cfg.Apps {
		if app.Name == name {
			return app, true
		}
	}
	return config.App{}, false
}

func (r *Registry) AppNames() ([]string, error) {
	apps, err := r.AllApps()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(apps))
	seen := map[string]struct{}{}
	for _, app := range apps {
		if _, ok := seen[app.Name]; ok {
			continue
		}
		seen[app.Name] = struct{}{}
		names = append(names, app.Name)
	}
	return names, nil
}

func (r *Registry) ValidateApp(app config.App) error {
	if err := app.Validate(); err != nil {
		return err
	}
	if app.Name == "" {
		return fmt.Errorf("name is required")
	}
	if _, ok := r.getStaticApp(app.Name); ok {
		return fmt.Errorf("app %q already exists in static config", app.Name)
	}
	if _, ok := r.st.GetDynamicApp(app.Name); ok {
		return fmt.Errorf("app %q already exists", app.Name)
	}
	return nil
}
