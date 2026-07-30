package netpolicy

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

// errResolver always fails LookupIPAddr.
type errResolver struct{ err error }

func (r errResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return nil, r.err
}

// emptyResolver succeeds with zero addresses.
type emptyResolver struct{}

func (emptyResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return nil, nil
}

// TestCov_DialContext_SplitHostPortError covers the bad-address branch (and the
// nil-Resolver → DefaultResolver assignment along the way).
func TestCov_DialContext_SplitHostPortError(t *testing.T) {
	d := &PolicyDialer{} // Resolver nil → DefaultResolver
	_, err := d.DialContext(context.Background(), "tcp", "no-port-here")
	assert.Error(t, err)
}

// TestCov_DialContext_LookupError covers the resolver-failure branch.
func TestCov_DialContext_LookupError(t *testing.T) {
	d := &PolicyDialer{Resolver: errResolver{err: errors.New("dns boom")}}
	_, err := d.DialContext(context.Background(), "tcp", "host.example:80")
	assert.Error(t, err)
}

// TestCov_DialContext_NoAddresses covers the "resolved to nothing" branch.
func TestCov_DialContext_NoAddresses(t *testing.T) {
	d := &PolicyDialer{Resolver: emptyResolver{}}
	_, err := d.DialContext(context.Background(), "tcp", "host.example:80")
	assert.Error(t, err)
}
