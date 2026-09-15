package main

import (
	"fmt"

	"github.com/x6nux/yanshi/internal/ctl"
)

// openCLICtl opens the OFFLINE control plane for a project: the store and the
// persisted approval rules, no daemon. Verbs whose state is stored (session,
// usage) go through it so they keep working when the backend is down — which is
// exactly when an operator reaches for them.
//
// It deliberately does not fall back to the daemon when the socket is missing:
// the stored answers are complete, and a silent switch would make the same
// command return different information depending on what happens to be running.
func openCLICtl(configPath string) (*ctl.Service, error) {
	svc, err := ctl.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("%w (is %s the right config?)", err, configPath)
	}
	return svc, nil
}
