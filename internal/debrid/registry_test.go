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
