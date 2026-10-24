package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDestructiveCommandsRequireYes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".cli-proxy-api"), 0o700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(os.Getenv("HOME"), ".cli-proxy-api", "claude-test.json")
	if err := os.WriteFile(credential, []byte("test credential"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"grants", "revoke", "id"},
		{"connections", "drop", "id"},
		{"providers", "disconnect", "claude"},
		{"app", "quit"},
		{"app", "restart"},
	} {
		if err := dispatch(nil, args, false); err == nil {
			t.Errorf("%v did not require --yes", args)
		}
	}
	if _, err := os.Stat(credential); err != nil {
		t.Fatalf("provider credential changed without --yes: %v", err)
	}
}

func TestResetSharingCLI(t *testing.T) {
	for _, args := range [][]string{{"set", "share-codex-resets", "yes"}, {"set", "share-codex-resets", "on", "extra"}, {"set", "share-usage", "bad"}} {
		if cmdConfig(nil, args, true) == nil {
			t.Fatal("invalid argument accepted")
		}
	}
}

func TestResetSharingCLISendsOnlyRequestedField(t *testing.T) {
	for _, on := range []string{"on", "off"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PUT" || r.URL.Path != "/api/config" {
				t.Fatal("wrong request")
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["shareCodexResetCredits"] != (on == "on") {
				t.Fatalf("bad patch: %+v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
		}))
		c := &client{base: server.URL, http: server.Client()}
		if err := cmdConfig(c, []string{"set", "share-codex-resets", on}, true); err != nil {
			t.Fatal(err)
		}
		server.Close()
	}
}
