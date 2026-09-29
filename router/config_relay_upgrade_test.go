package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLegacyDefaultRelaysGainWorkingBackupRelays(t *testing.T) {
	for _, old := range legacyDefaultRelayLists {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := saveJSON(path, Config{NostrRelays: old}); err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		store, err := openStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := store.Config().NostrRelays
		if !slices.Equal(got, defaultNostrRelays) {
			t.Fatalf("legacy list %v upgraded to %v", old, got)
		}
		backup, err := os.ReadFile(path + ".pre-1.1.1")
		if err != nil || !bytes.Equal(backup, original) {
			t.Fatalf("original config was not backed up exactly: %v", err)
		}
		again, err := openStore(dir)
		if err != nil || !slices.Equal(again.Config().NostrRelays, defaultNostrRelays) {
			t.Fatalf("upgrade was not idempotent: %v", err)
		}
	}
}

func TestRelayUpgradePreservesUnknownConfigFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	fields := map[string]any{
		"nostrRelays":   legacyDefaultRelayLists[0],
		"futureSetting": map[string]any{"value": 3},
	}
	if err := saveJSON(path, fields); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var future struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal(got["futureSetting"], &future); err != nil || future.Value != 3 {
		t.Fatalf("unknown config field was lost: %s", got["futureSetting"])
	}
}

func TestInvalidSavedStateIsNeverReplacedOnStartup(t *testing.T) {
	for _, name := range []string{"config.json", "grants.json", "connections.json", "usage.json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			original := []byte(`{"incomplete":`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := openStore(dir); err == nil {
				t.Fatal("startup accepted malformed saved state")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("saved state changed after failed load: %v", err)
			}
		})
	}
}

func TestCustomRelaysStayUnchanged(t *testing.T) {
	custom := []string{"wss://nos.lol", "wss://relay.damus.io", "wss://my-relay.example"}
	got := upgradeDefaultRelays(slices.Clone(custom))
	if !slices.Equal(got, custom) {
		t.Fatalf("custom list changed from %v to %v", custom, got)
	}
	if got := upgradeDefaultRelays(slices.Clone(defaultNostrRelays)); !slices.Equal(got, defaultNostrRelays) {
		t.Fatalf("current defaults changed on repeat upgrade: %v", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := saveJSON(path, Config{NostrRelays: custom}); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	if _, err := openStore(dir); err != nil {
		t.Fatal(err)
	}
	still, _ := os.ReadFile(path)
	if !bytes.Equal(still, original) {
		t.Fatal("opening a custom config rewrote it")
	}
}

func TestRelayUpgradePreservesPopulatedUserState(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{NostrRelays: legacyDefaultRelayLists[0], IdentityName: "existing host", ShareUsageLevel: shareUsageOff, EnableSharing: true}
	files := map[string]any{
		"config.json":      cfg,
		"grants.json":      []Grant{{ID: "existing-grant", Code: "secret-grant", TokenLimit: 12345, Paused: true}},
		"connections.json": []Connection{{ID: "existing-connection", Code: "secret-borrow", Label: "friend"}},
		"usage.json":       map[string]Usage{"existing-grant": {Requests: 7, InputTokens: 300, OutputTokens: 400}},
	}
	originals := map[string][]byte{}
	for name, value := range files {
		path := filepath.Join(dir, name)
		if err := saveJSON(path, value); err != nil {
			t.Fatal(err)
		}
		originals[name], _ = os.ReadFile(path)
	}
	for i := 0; i < 2; i++ {
		store, err := openStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if store.Config().IdentityName != cfg.IdentityName || store.Config().shareUsageLevel() != shareUsageOff {
			t.Fatal("identity or privacy changed")
		}
		for name, original := range originals {
			if name == "config.json" {
				continue
			}
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("%s changed on startup: %v", name, err)
			}
		}
	}
	backup, err := os.ReadFile(filepath.Join(dir, "config.json.pre-1.1.1"))
	if err != nil || !bytes.Equal(backup, originals["config.json"]) {
		t.Fatal("rollback config did not preserve original")
	}
}
