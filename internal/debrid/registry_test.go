package debrid

import (
	"reflect"
	"testing"
)

func TestRegistry_RegistersPerKindAndKeepsOrder(t *testing.T) {
	r := NewRegistry()
	r.Register("torbox",
		NewDynamicTorrentProvider("torbox"),
		NewDynamicUsenetProvider("torbox"),
		NewDynamicWebDownloadProvider("torbox"))
	// A torrent-only provider registers nothing for the other two kinds.
	r.Register("torrents-only", NewDynamicTorrentProvider("torrents-only"), nil, nil)

	if got, want := r.Names(), []string{"torbox", "torrents-only"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v (registration order)", got, want)
	}
	if got, want := r.TorrentNames(), []string{"torbox", "torrents-only"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TorrentNames() = %v, want %v", got, want)
	}
	if got, want := r.UsenetNames(), []string{"torbox"}; !reflect.DeepEqual(got, want) {
		t.Errorf("UsenetNames() = %v, want %v — a torrent-only provider must not appear", got, want)
	}
	if r.Usenet("torrents-only") != nil {
		t.Error("Usenet(\"torrents-only\") returned a wrapper for a kind it doesn't support")
	}
	if r.Torrent("nope") != nil {
		t.Error("Torrent() returned a wrapper for an unregistered provider")
	}
}

func TestRegistry_DefaultsToFirstRegistered(t *testing.T) {
	r := NewRegistry()
	if r.Default() != "" {
		t.Errorf("Default() = %q on an empty registry, want empty", r.Default())
	}

	r.Register("first", NewDynamicTorrentProvider("first"), nil, nil)
	r.Register("second", NewDynamicTorrentProvider("second"), nil, nil)
	if r.Default() != "first" {
		t.Errorf("Default() = %q, want \"first\" — a single-provider install should need no configuration", r.Default())
	}

	r.SetDefault("second")
	if r.Default() != "second" {
		t.Errorf("Default() = %q after SetDefault, want \"second\"", r.Default())
	}

	// Driven by a persisted setting, so a name that is no longer configured
	// must not blank out a working default.
	r.SetDefault("removed-provider")
	if r.Default() != "second" {
		t.Errorf("Default() = %q after SetDefault to an unregistered name, want it unchanged", r.Default())
	}
}

func TestRegistry_ReregisteringKeepsPosition(t *testing.T) {
	r := NewRegistry()
	r.Register("a", NewDynamicTorrentProvider("a"), nil, nil)
	r.Register("b", NewDynamicTorrentProvider("b"), nil, nil)

	replacement := NewDynamicTorrentProvider("a")
	r.Register("a", replacement, nil, nil)

	if got, want := r.Names(), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v — re-registering must not duplicate or reorder", got, want)
	}
	if r.Torrent("a") != replacement {
		t.Error("Torrent(\"a\") did not return the replacement wrapper")
	}
}

// TestRegistry_DefaultFallsBackPerKind is the fix for a real failure: the
// default provider is one setting across every kind, but providers differ
// in what they support. With a torrent-only provider as the default, usenet
// resolved to it and every usenet add failed — even with a usenet-capable
// provider configured right beside it. Found live by making AllDebrid the
// default and watching SABnzbd break.
func TestRegistry_DefaultFallsBackPerKind(t *testing.T) {
	r := NewRegistry()
	r.Register("full",
		NewDynamicTorrentProvider("full"),
		NewDynamicUsenetProvider("full"),
		NewDynamicWebDownloadProvider("full"))
	r.Register("torrents-only", NewDynamicTorrentProvider("torrents-only"), nil, nil)

	r.SetDefault("torrents-only")

	// Torrents honour the default.
	if got := r.DefaultNameFor(KindTorrent); got != "torrents-only" {
		t.Errorf("DefaultNameFor(torrent) = %q, want torrents-only", got)
	}
	if r.DefaultTorrent() == nil {
		t.Error("DefaultTorrent() = nil")
	}

	// Usenet and webdl fall back to a provider that can actually do them,
	// rather than resolving to the default and failing.
	if got := r.DefaultNameFor(KindUsenet); got != "full" {
		t.Errorf("DefaultNameFor(usenet) = %q, want full — the default can't do usenet", got)
	}
	if r.DefaultUsenet() == nil {
		t.Error("DefaultUsenet() = nil despite a usenet-capable provider being registered")
	}
	if got := r.DefaultNameFor(KindWebDL); got != "full" {
		t.Errorf("DefaultNameFor(webdl) = %q, want full", got)
	}
	if r.DefaultWebDL() == nil {
		t.Error("DefaultWebDL() = nil despite a webdl-capable provider being registered")
	}
}

// With nothing registered for a kind at all, there is genuinely nothing to
// fall back to and callers need a nil to report "not configured".
func TestRegistry_DefaultForUnsupportedKindIsNil(t *testing.T) {
	r := NewRegistry()
	r.Register("torrents-only", NewDynamicTorrentProvider("torrents-only"), nil, nil)

	if r.DefaultUsenet() != nil {
		t.Error("DefaultUsenet() non-nil with no usenet provider registered")
	}
	if got := r.DefaultNameFor(KindUsenet); got != "" {
		t.Errorf("DefaultNameFor(usenet) = %q, want empty", got)
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	r.Register("a", NewDynamicTorrentProvider("a"), nil, nil)
	r.Register("b", NewDynamicTorrentProvider("b"), nil, nil)
	r.SetDefault("a")

	r.Unregister("a")

	if got, want := r.Names(), []string{"b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	if r.Torrent("a") != nil {
		t.Error("Torrent(\"a\") still resolves after Unregister")
	}
	// Removing the default must hand it to whatever remains, or an add
	// straight afterwards would resolve to a name that no longer exists.
	if r.Default() != "b" {
		t.Errorf("Default() = %q after removing the default, want b", r.Default())
	}

	// Removing something that was never there is a no-op, not a panic.
	r.Unregister("never-existed")
	if got, want := r.Names(), []string{"b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v after removing an unknown name, want %v", got, want)
	}
}
