// Command vibeshare controls a running VibeShare menu-bar app over its loopback
// control API (127.0.0.1:8799). Agents (Claude Code, Codex, Hermes, Grok) use
// it for every share, borrow, and settings action the menu exposes.
//
// The grant code, borrowed connections, and provider sign-ins are never
// rewritten unless a command says so. Destructive commands require --yes.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const cliVersion = "1.1.2"

func main() {
	args := os.Args[1:]
	jsonOut := os.Getenv("VIBESHARE_JSON") == "1"
	port := 0
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			jsonOut = true
		case a == "--help" || a == "-h" || a == "help":
			usage(os.Stdout)
			return
		case a == "--version" || a == "version":
			fmt.Println(cliVersion)
			return
		case a == "--port" && i+1 < len(args):
			i++
			port, _ = strconv.Atoi(args[i])
		case strings.HasPrefix(a, "--port="):
			port, _ = strconv.Atoi(strings.TrimPrefix(a, "--port="))
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		usage(os.Stderr)
		os.Exit(2)
	}
	c, err := newClient(port)
	if err != nil {
		fail(err, jsonOut)
	}
	if err := dispatch(c, rest, jsonOut); err != nil {
		fail(err, jsonOut)
	}
}

func fail(err error, jsonOut bool) {
	if jsonOut {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"error": err.Error(), "ok": false})
	} else {
		fmt.Fprintf(os.Stderr, "vibeshare: %s\n", err)
	}
	os.Exit(1)
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `vibeshare %s — control the VibeShare app on this Mac

Talks to the running app at 127.0.0.1:8799. Works with the 1.0 control API, so
an agent can drive a friend who has not installed 1.1 yet. Pass --json for a
stable JSON result on stdout. Set VIBESHARE_JSON=1 for the same thing.

  vibeshare status
  vibeshare doctor                       app, router, provider engine health
  vibeshare models                       models available at the local endpoint
  vibeshare env                          shell exports for Claude / Codex
  vibeshare activity [N]
  vibeshare logs [router|provider|login-<key>]
  vibeshare sharing on|off               master switch (also pauses lending)
  vibeshare config
  vibeshare config set identity <name>
  vibeshare config set auto-stop on|off
  vibeshare config set prefer-borrowed on|off
  vibeshare config set share-codex-resets on|off
  vibeshare config set share-usage off|resets|windows
  vibeshare config set reserve <1-95>
  vibeshare config set relays <wss-url>...
  vibeshare grants
  vibeshare grants create --label <name> [--provider <key>]... [--model <id>]... [--limit <tokens>]
  vibeshare grants pause <id>
  vibeshare grants resume <id>
  vibeshare grants limit <id> <tokens>   0 = unlimited
  vibeshare grants revoke <id> --yes
  vibeshare connections
  vibeshare connections redeem --code <code> [--label <name>]
  vibeshare connections drop <id> --yes
  vibeshare providers
  vibeshare providers login <key>        opens that provider's sign-in
  vibeshare providers disconnect <key> --yes
  vibeshare usage refresh <key>
  vibeshare usage                         all available subscription bars
  vibeshare app open|quit|restart         quit/restart require --yes

Provider keys: claude, codex, gemini, kimi, antigravity, xai.
Lending to a friend is "grants". Using a friend's models is "connections".
Pausing one grant stops lending to that friend and leaves borrows alone.
`, cliVersion)
}

func dispatch(c *client, args []string, jsonOut bool) error {
	switch args[0] {
	case "status":
		return c.show("GET", "/api/status", nil, jsonOut, printStatus)
	case "doctor":
		return doctor(c, jsonOut)
	case "models":
		return listModels(c, jsonOut)
	case "logs":
		return showLogs(args[1:], jsonOut)
	case "app":
		return cmdApp(args[1:], jsonOut)
	case "env":
		return printEnv(c, jsonOut)
	case "activity":
		n := "50"
		if len(args) > 1 {
			n = args[1]
		}
		return c.show("GET", "/api/activity?limit="+n, nil, jsonOut, nil)
	case "sharing":
		if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
			return errors.New("usage: vibeshare sharing on|off")
		}
		return c.show("PUT", "/api/config", map[string]any{"enableSharing": args[1] == "on"}, jsonOut, nil)
	case "config":
		return cmdConfig(c, args[1:], jsonOut)
	case "grants":
		return cmdGrants(c, args[1:], jsonOut)
	case "connections":
		return cmdConnections(c, args[1:], jsonOut)
	case "providers":
		return cmdProviders(c, args[1:], jsonOut)
	case "usage":
		if len(args) == 1 {
			st, _, err := c.get("/api/status")
			if err != nil {
				return err
			}
			return printJSON(st["providerUsage"])
		}
		if len(args) != 3 || args[1] != "refresh" {
			return errors.New("usage: vibeshare usage [refresh <claude|codex>]")
		}
		if args[2] != "claude" && args[2] != "codex" {
			return errors.New("usage refresh supports claude and codex")
		}
		return c.show("POST", "/api/usage/"+args[2]+"/refresh", nil, jsonOut, nil)
	default:
		return fmt.Errorf("unknown command %q (vibeshare help)", args[0])
	}
}

func cmdConfig(c *client, args []string, jsonOut bool) error {
	if len(args) == 0 {
		return c.show("GET", "/api/config", nil, jsonOut, nil)
	}
	if args[0] != "set" || len(args) < 3 {
		return errors.New("usage: vibeshare config set identity|auto-stop|prefer-borrowed|share-codex-resets|share-usage|reserve|relays ...")
	}
	var body map[string]any
	switch args[1] {
	case "identity":
		body = map[string]any{"identityName": strings.Join(args[2:], " ")}
	case "auto-stop":
		if args[2] != "on" && args[2] != "off" {
			return errors.New("auto-stop expects on or off")
		}
		body = map[string]any{"autoStopSharing": args[2] == "on"}
	case "prefer-borrowed":
		if args[2] != "on" && args[2] != "off" {
			return errors.New("prefer-borrowed expects on or off")
		}
		body = map[string]any{"preferBorrowedModels": args[2] == "on"}
	case "share-codex-resets":
		if len(args) != 3 || (args[2] != "on" && args[2] != "off") {
			return errors.New("share-codex-resets expects on or off")
		}
		body = map[string]any{"shareCodexResetCredits": args[2] == "on"}
	case "share-usage":
		if len(args) != 3 || (args[2] != "off" && args[2] != "resets" && args[2] != "windows") {
			return errors.New("share-usage expects off, resets, or windows")
		}
		body = map[string]any{"shareUsageLevel": args[2]}
	case "reserve":
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 1 || n > 95 {
			return errors.New("reserve expects a percent from 1 to 95")
		}
		body = map[string]any{"usageReservePercent": n}
	case "relays":
		body = map[string]any{"nostrRelays": args[2:]}
	default:
		return fmt.Errorf("unknown config field %q", args[1])
	}
	if err := c.show("PUT", "/api/config", body, jsonOut, nil); err != nil {
		return err
	}
	if args[1] == "relays" && !jsonOut {
		fmt.Println("relay list saved. Restart VibeShare (or its router) for the new relays to connect.")
	}
	return nil
}

func cmdGrants(c *client, args []string, jsonOut bool) error {
	if len(args) == 0 {
		return c.show("GET", "/api/grants", nil, jsonOut, nil)
	}
	switch args[0] {
	case "create":
		body, err := grantCreateBody(args[1:])
		if err != nil {
			return err
		}
		return c.show("POST", "/api/grants", body, jsonOut, nil)
	case "pause", "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: vibeshare grants %s <id>", args[0])
		}
		return c.show("PATCH", "/api/grants/"+args[1], map[string]any{"paused": args[0] == "pause"}, jsonOut, nil)
	case "limit":
		if len(args) != 3 {
			return errors.New("usage: vibeshare grants limit <id> <tokens>")
		}
		n, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || n < 0 {
			return errors.New("limit expects a token count, 0 for unlimited")
		}
		return c.show("PATCH", "/api/grants/"+args[1], map[string]any{"tokenLimit": n}, jsonOut, nil)
	case "revoke":
		if len(args) != 3 || args[2] != "--yes" {
			return errors.New("usage: vibeshare grants revoke <id> --yes  (this kills the code)")
		}
		return c.show("DELETE", "/api/grants/"+args[1], nil, jsonOut, nil)
	default:
		return fmt.Errorf("unknown grants command %q", args[0])
	}
}

func grantCreateBody(args []string) (map[string]any, error) {
	body := map[string]any{"label": "", "providers": []string{}, "models": []string{}, "tokenLimit": 0}
	var providers, models []string
	for i := 0; i < len(args); i++ {
		take := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", args[i])
			}
			i++
			return args[i], nil
		}
		switch args[i] {
		case "--label":
			v, err := take()
			if err != nil {
				return nil, err
			}
			body["label"] = v
		case "--provider":
			v, err := take()
			if err != nil {
				return nil, err
			}
			providers = append(providers, v)
		case "--model":
			v, err := take()
			if err != nil {
				return nil, err
			}
			models = append(models, v)
		case "--limit":
			v, err := take()
			if err != nil {
				return nil, err
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return nil, errors.New("--limit expects a token count")
			}
			body["tokenLimit"] = n
		default:
			return nil, fmt.Errorf("unknown flag %s", args[i])
		}
	}
	if body["label"] == "" {
		return nil, errors.New("grants create needs --label")
	}
	body["providers"] = providers
	body["models"] = models
	return body, nil
}

func cmdConnections(c *client, args []string, jsonOut bool) error {
	if len(args) == 0 {
		return c.show("GET", "/api/connections", nil, jsonOut, nil)
	}
	switch args[0] {
	case "redeem":
		var code, label string
		for i := 1; i < len(args); i++ {
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", args[i])
			}
			switch args[i] {
			case "--code":
				i++
				code = args[i]
			case "--label":
				i++
				label = args[i]
			default:
				return fmt.Errorf("unknown flag %s", args[i])
			}
		}
		if code == "" {
			return errors.New("connections redeem needs --code")
		}
		return c.show("POST", "/api/connections", map[string]any{"code": code, "label": label}, jsonOut, nil)
	case "drop":
		if len(args) != 3 || args[2] != "--yes" {
			return errors.New("usage: vibeshare connections drop <id> --yes")
		}
		return c.show("DELETE", "/api/connections/"+args[1], nil, jsonOut, nil)
	default:
		return fmt.Errorf("unknown connections command %q", args[0])
	}
}

type providerInfo struct {
	key, name, loginFlag string
	hints                []string
	prefixes             []string
}

func knownProviders() []providerInfo {
	return []providerInfo{
		{"claude", "Claude (Anthropic)", "-claude-login", []string{"claude"}, []string{"claude"}},
		{"codex", "ChatGPT / Codex", "-codex-login", []string{"gpt", "o1", "o3", "o4", "codex"}, []string{"codex", "chatgpt"}},
		{"gemini", "Gemini (Google)", "-login", []string{"gemini"}, []string{"gemini"}},
		{"kimi", "Kimi (Moonshot)", "-kimi-login", []string{"kimi", "moonshot"}, []string{"kimi"}},
		{"antigravity", "Antigravity", "-antigravity-login", []string{"gemini", "antigravity"}, []string{"antigravity"}},
		{"xai", "xAI / Grok", "-xai-login", []string{"grok"}, []string{"xai"}},
	}
}

func findProvider(key string) (providerInfo, error) {
	for _, p := range knownProviders() {
		if p.key == key {
			return p, nil
		}
	}
	return providerInfo{}, fmt.Errorf("unknown provider %q", key)
}

func cmdProviders(c *client, args []string, jsonOut bool) error {
	if len(args) == 0 {
		status, raw, err := c.get("/api/status")
		if err != nil {
			return err
		}
		models := stringList(status["localModels"])
		out := make([]map[string]any, 0, len(knownProviders()))
		for _, p := range knownProviders() {
			out = append(out, map[string]any{
				"key": p.key, "name": p.name, "connected": providerConnected(p, models),
			})
		}
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}
		_ = raw
		for _, row := range out {
			state := "not connected"
			if row["connected"].(bool) {
				state = "connected"
			}
			fmt.Printf("%-12s %s\n", row["key"], state)
		}
		return nil
	}
	if len(args) < 2 {
		return errors.New("usage: vibeshare providers [login|disconnect] <key>")
	}
	p, err := findProvider(args[1])
	if err != nil {
		return err
	}
	switch args[0] {
	case "login":
		if jsonOut {
			return errors.New("provider sign-in is interactive; run without --json")
		}
		return loginProvider(p)
	case "disconnect":
		if len(args) != 3 || args[2] != "--yes" {
			return errors.New("usage: vibeshare providers disconnect <key> --yes")
		}
		return disconnectProvider(p, jsonOut)
	default:
		return fmt.Errorf("unknown providers command %q", args[0])
	}
}

func providerConnected(p providerInfo, models []string) bool {
	for _, id := range models {
		lid := strings.ToLower(id)
		for _, hint := range p.hints {
			if strings.Contains(lid, hint) {
				return true
			}
		}
	}
	return false
}

func loginProvider(p providerInfo) error {
	bin, err := providerBinary()
	if err != nil {
		return err
	}
	cfg := filepath.Join(stateDir(), "cli-proxy-config.yaml")
	fmt.Printf("starting %s sign-in. Finish it in the browser; this command waits.\n", p.name)
	cmd := exec.Command(bin, p.loginFlag, "-config", cfg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func disconnectProvider(p providerInfo, jsonOut bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".cli-proxy-api")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var removed []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		for _, prefix := range p.prefixes {
			if strings.HasPrefix(name, prefix+"-") {
				path := filepath.Join(dir, name)
				if err := os.Remove(path); err != nil {
					return err
				}
				removed = append(removed, path)
			}
		}
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"removed": removed})
	}
	if len(removed) == 0 {
		fmt.Printf("no stored sign-in for %s\n", p.key)
		return nil
	}
	fmt.Printf("removed %d %s sign-in file(s)\n", len(removed), p.key)
	return nil
}

func providerBinary() (string, error) {
	if v := os.Getenv("VIBESHARE_PROVIDER_BIN"); v != "" {
		return v, nil
	}
	candidates := []string{
		"/Applications/VibeShare.app/Contents/Resources/cli-proxy-api-plus",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "cli-proxy-api-plus"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("cli-proxy-api-plus not found. Set VIBESHARE_PROVIDER_BIN or install VibeShare.app")
}

func printEnv(c *client, jsonOut bool) error {
	st, _, err := c.get("/api/status")
	port := 8788
	if err == nil {
		if n, ok := st["frontPort"].(float64); ok && n > 0 {
			port = int(n)
		}
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"ANTHROPIC_BASE_URL": base,
			"ANTHROPIC_API_KEY":  "vibeshare",
			"OPENAI_BASE_URL":    base + "/v1",
			"OPENAI_API_KEY":     "vibeshare",
		})
	}
	fmt.Printf("ANTHROPIC_BASE_URL=%s ANTHROPIC_API_KEY=vibeshare\n", base)
	fmt.Printf("OPENAI_BASE_URL=%s/v1 OPENAI_API_KEY=vibeshare\n", base)
	return nil
}

func printStatus(body []byte) {
	var st map[string]any
	if json.Unmarshal(body, &st) != nil {
		os.Stdout.Write(body)
		return
	}
	ver, _ := st["version"].(string)
	if ver == "" {
		ver = "older than 1.1.0"
	}
	sharing := "off"
	if st["sharingEnabled"] == true {
		sharing = "on"
	}
	fmt.Printf("router %s  sharing %s  %s\n", ver, sharing, st["identityName"])
	if st["preferBorrowedModels"] == true {
		fmt.Println("routing priority: borrowed models")
	}
	if up, ok := st["upstream"].(map[string]any); ok {
		reach := "down"
		if up["reachable"] == true {
			reach = "up"
		}
		fmt.Printf("provider engine %s\n", reach)
	}
	if nostr, ok := st["nostr"].(map[string]any); ok {
		if relays, ok := nostr["relays"].([]any); ok {
			usable := 0
			for _, raw := range relays {
				if row, ok := raw.(map[string]any); ok && row["connected"] == true && row["coolingDown"] != true {
					usable++
				}
			}
			fmt.Printf("relays %d/%d usable\n", usable, len(relays))
		}
	}
	fmt.Printf("endpoint http://127.0.0.1:%v/v1\n", st["frontPort"])
}

func stringList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

type client struct {
	base string
	http *http.Client
}

func newClient(port int) (*client, error) {
	if port == 0 {
		port = readControlPort()
	}
	if port == 0 {
		port = 8799
	}
	return &client{
		base: fmt.Sprintf("http://127.0.0.1:%d", port),
		http: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func readControlPort() int {
	b, err := os.ReadFile(filepath.Join(stateDir(), "config.json"))
	if err != nil {
		return 0
	}
	var cfg struct {
		ControlPort int `json:"controlPort"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return 0
	}
	return cfg.ControlPort
}

func stateDir() string {
	if v := os.Getenv("VIBESHARE_STATE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".vibeshare"
	}
	return filepath.Join(home, ".vibeshare")
}

func (c *client) show(method, path string, body any, jsonOut bool, human func([]byte)) error {
	raw, err := c.do(method, path, body)
	if err != nil {
		return err
	}
	if jsonOut || human == nil {
		var buf bytes.Buffer
		if json.Indent(&buf, raw, "", "  ") != nil {
			os.Stdout.Write(raw)
			return nil
		}
		fmt.Println(buf.String())
		return nil
	}
	human(raw)
	return nil
}

func (c *client) get(path string) (map[string]any, []byte, error) {
	raw, err := c.do("GET", path, nil)
	if err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, raw, err
	}
	return m, raw, nil
}

func (c *client) do(method, path string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control API unreachable (%s). Is VibeShare running?", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		return nil, errors.New(msg)
	}
	if len(b) == 0 {
		b = []byte("{}\n")
	}
	return b, nil
}
