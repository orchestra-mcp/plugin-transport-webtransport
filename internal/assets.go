package internal

import "embed"

//go:embed dist
var dashboardFS embed.FS

// DashboardFS is the embedded React dashboard build output.
// It is populated by `make build-dashboard` which copies apps/web/dist/
// into this directory before the Go binary is compiled.
var DashboardFS = dashboardFS
