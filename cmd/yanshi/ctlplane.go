package main

import (
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/ctl"
)

// appControlPlane builds the operator control plane over an assembled App.
//
// It is ONE constructor for every surface that needs it (`yanshi app`, the IPC
// socket), because the alternative — each surface picking the managers it
// happens to know about — is how two clients end up disagreeing about which
// capabilities exist.
//
// Every manager is passed as-is, nil included: ctl reports ErrNeedsDaemon for a
// domain whose manager is missing (a soft-degraded MCP subsystem, a VCS that
// failed InitRepo), which is the truth a caller needs. Substituting an empty
// manager here would turn "this daemon cannot see it" into "there is nothing".
func appControlPlane(app *bootstrap.App) *ctl.Service {
	models := make([]string, 0, len(app.Models))
	for name := range app.Models {
		models = append(models, name)
	}
	return ctl.New(ctl.Deps{
		Store:     app.Store,
		Skills:    app.Skills,
		MCP:       app.MCP,
		Jobs:      app.ShellManager,
		Features:  app.Features,
		Approvals: app.Approvals,
		VCS:       app.VCS,
		VCSRepoID: app.VCSRepoID,
		Models:    models,
	})
}
