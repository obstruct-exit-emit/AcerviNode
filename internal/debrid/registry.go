package debrid

import "sync"

// Registry is every configured provider, indexed by name.
//
// Each Dynamic*Provider stays exactly what it was — the swappable slot for
// one provider's credentials, and the owner of that provider's shared
// listing cache (see ListCache). What changes is that there can now be more
// than one of them per kind, so "which provider" is a lookup rather than a
// field.
//
// A provider registers only the kinds it actually supports: TorBox happens
// to do all three, but a torrent-only service registers just the torrent
// wrapper and is simply absent from the usenet and webdl maps. That is why
// the accessors return a typed pointer rather than an interface — a nil
// *DynamicTorrentProvider stored in an interface is not a nil interface,
// and every caller here decides what to do when a provider isn't there.
//
// Deliberately knows nothing about database.Kind: internal/debrid sits
// below internal/database, and taking a Kind here would invert that. Callers
// already switch on kind for their own reasons and pick the accessor.
type Registry struct {
	mu      sync.RWMutex
	torrent map[string]*DynamicTorrentProvider
	usenet  map[string]*DynamicUsenetProvider
	webdl   map[string]*DynamicWebDownloadProvider
	// order is registration order, so Names and any iteration built on it
	// are stable rather than following Go's randomised map order. Callers
	// surface this to users (GET /api/v1/providers), where a list that
	// reshuffles between requests would be its own small bug.
	order []string
	// fallback is the provider new downloads go to when nothing else
	// decides — see Default. Set explicitly; otherwise the first registered
	// provider, so a single-provider install needs no configuration at all.
	fallback string
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		torrent: map[string]*DynamicTorrentProvider{},
		usenet:  map[string]*DynamicUsenetProvider{},
		webdl:   map[string]*DynamicWebDownloadProvider{},
	}
}

// Register adds a provider under name, for whichever kinds it supports.
// Pass nil for a kind this provider can't do. Registering the same name
// twice replaces its wrappers but keeps its position in the ordering.
func (r *Registry) Register(name string, t *DynamicTorrentProvider, u *DynamicUsenetProvider, w *DynamicWebDownloadProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, seen := r.indexOf(name); !seen {
		r.order = append(r.order, name)
	}
	if t != nil {
		r.torrent[name] = t
	}
	if u != nil {
		r.usenet[name] = u
	}
	if w != nil {
		r.webdl[name] = w
	}
	if r.fallback == "" {
		r.fallback = name
	}
}

// indexOf reports a name's position in order. Callers hold r.mu.
func (r *Registry) indexOf(name string) (int, bool) {
	for i, n := range r.order {
		if n == name {
			return i, true
		}
	}
	return 0, false
}

// Torrent returns the named provider's torrent wrapper, or nil if that
// provider isn't registered or doesn't do torrents.
func (r *Registry) Torrent(name string) *DynamicTorrentProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.torrent[name]
}

// Usenet is Torrent's usenet counterpart.
func (r *Registry) Usenet(name string) *DynamicUsenetProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.usenet[name]
}

// WebDL is Torrent's web-download counterpart.
func (r *Registry) WebDL(name string) *DynamicWebDownloadProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.webdl[name]
}

// SetDefault names the provider new downloads go to when nothing else
// decides. A name that isn't registered is ignored rather than erroring:
// this is driven by a persisted setting, and a provider named there may
// simply not be configured any more.
func (r *Registry) SetDefault(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.indexOf(name); ok {
		r.fallback = name
	}
}

// Default is the provider name new downloads go to — see SetDefault. Empty
// only when nothing is registered at all.
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fallback
}

// Names lists every registered provider in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// TorrentNames lists the registered providers that support torrents, in
// registration order — for the polling loops, which need every provider of
// a kind rather than one.
func (r *Registry) TorrentNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesWith(func(n string) bool { _, ok := r.torrent[n]; return ok })
}

// UsenetNames is TorrentNames' usenet counterpart.
func (r *Registry) UsenetNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesWith(func(n string) bool { _, ok := r.usenet[n]; return ok })
}

// WebDLNames is TorrentNames' web-download counterpart.
func (r *Registry) WebDLNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesWith(func(n string) bool { _, ok := r.webdl[n]; return ok })
}

// namesWith filters order by keep, preserving it. Callers hold r.mu.
func (r *Registry) namesWith(keep func(string) bool) []string {
	var out []string
	for _, n := range r.order {
		if keep(n) {
			out = append(out, n)
		}
	}
	return out
}
