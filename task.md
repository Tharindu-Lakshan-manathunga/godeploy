# GoDeploy UI Overhaul — Tasks

- `[x]` **Backend Auth Upgrades**
  - `[x]` Add Token field to loginResponse in types.go
  - `[x]` Include Token payload in handleLogin api.go
- `[x]` **CSS — Design System Redesign**
  - `[x]` Apply obsidian carbon palette (#07090e)
  - `[x]` Redesign sidebar layout with docked navigation items
- `[x]` **HTML — Custom SVG & Custom controls**
  - `[x]` Replace basic SVGs with customized developer tools icons
  - `[x]` Add digital oscilloscope screen grid overlay to telemetry graph
  - `[x]` Circular progress gauges for hardware allocation
- `[x]` **JS — Fallback Token Auth & WS Queries**
  - `[x]` Store token in localStorage during login
  - `[x]` Add Bearer headers to API requests
  - `[x]` Send token query parameter inside WS connection string
  - `[x]` Syntax highlighters for terminal log text
- `[x]` **Compilation**
  - `[x]` Compile godeploy.exe (Windows)
  - `[x]` Cross-compile Linux binary (godeploy)
