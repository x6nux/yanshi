// Package plugin hosts access connectors (Web/IM). Built-in connectors register
// directly; go-plugin process loading is stubbed here for M4+.
package plugin

import (
	"context"
	"fmt"
)

// Connector adapts an external channel (Web/IM) to yanshi's HTTP API.
// Implementations may be built-in (in-process) or, later, go-plugin processes.
type Connector interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Host holds registered connectors.
type Host struct {
	connectors map[string]Connector
}

// NewHost returns an empty Host.
func NewHost() *Host {
	return &Host{connectors: map[string]Connector{}}
}

// Register validates and adds a connector to the host.
func (h *Host) Register(c Connector) error {
	if c == nil || c.Name() == "" {
		return fmt.Errorf("plugin: invalid connector")
	}
	h.connectors[c.Name()] = c
	return nil
}

// Get returns a connector by name.
func (h *Host) Get(name string) (Connector, bool) {
	c, ok := h.connectors[name]
	return c, ok
}

// All returns every registered connector.
func (h *Host) All() []Connector {
	out := make([]Connector, 0, len(h.connectors))
	for _, c := range h.connectors {
		out = append(out, c)
	}
	return out
}
