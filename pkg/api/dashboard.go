package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dashboard
var dashboardFS embed.FS

// DashboardHandler returns an http.FileSystem serving the embedded dashboard files.
func DashboardHandler() http.FileSystem {
	sub, err := fs.Sub(dashboardFS, "dashboard")
	if err != nil {
		panic("embedded dashboard missing: " + err.Error())
	}
	return http.FS(sub)
}
