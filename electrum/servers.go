package electrum

import "sync/atomic"

// ServerProvider manages a rotating server list, from which callers can pull server addresses.
type ServerProvider struct {
	nextIndex int32
	servers   []string
}

// NewServerProvider returns an initialized ServerProvider.
func NewServerProvider(servers []string) *ServerProvider {
	return &ServerProvider{
		nextIndex: -1,
		servers:   servers,
	}
}

// NextServer returns an address from the rotating list. It's thread-safe.
func (p *ServerProvider) NextServer() string {
	index := int(atomic.AddInt32(&p.nextIndex, 1))
	return p.servers[index%len(p.servers)]
}

// PublicServers list.
//
// This list was taken from Electrum repositories, keeping TLS servers and excluding onion URIs.
// It was then sorted into sections using the `cmd/survey` program, to prioritize the more reliable
// servers with batch support.
//
// See https://github.com/spesmilo/electrum/blob/master/electrum/servers.json
// See https://github.com/kyuupichan/electrumx/blob/master/electrumx/lib/coins.py
// See `cmd/survey/main.go`
var PublicServers = []string{
	// Fast servers with batching
	"fulcrum.sethforprivacy.com:50002",      // impl: Fulcrum 1.9.0, batching: true, ttc: 0.55, speed: 96, from:

	// Other servers
}
