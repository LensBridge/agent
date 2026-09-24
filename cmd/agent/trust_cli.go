package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/enroll"
	"github.com/LensBridge/agent/internal/trust"
)

// runTrust is `trust show|add|remove|fetch` (docs/architecture.md, section
// 5). trust.json is root-owned and the daemon only reads it; it reloads the
// keys for every import, so changes here take effect without a restart.
func runTrust(args []string) {
	if len(args) == 0 {
		trustUsage()
	}
	switch args[0] {
	case "show":
		if len(args) != 1 {
			trustUsage()
		}
		trustShow()
	case "add":
		if len(args) < 3 || len(args) > 4 {
			trustUsage()
		}
		requireRoot("trust add")
		note := ""
		if len(args) == 4 {
			note = args[3]
		}
		trustAdd(args[1], args[2], note)
	case "remove":
		if len(args) != 2 {
			trustUsage()
		}
		requireRoot("trust remove")
		trustRemove(args[1])
	case "fetch":
		trustFetch(args[1:])
	default:
		trustUsage()
	}
}

func trustUsage() {
	fmt.Fprint(os.Stderr, `usage:
  musallahboard-agent trust show                          List the keys this board accepts packages from
  musallahboard-agent trust add content|release <b64> [note]   Trust another public key (root)
  musallahboard-agent trust remove <keyId>                Stop trusting a key (root)
  musallahboard-agent trust fetch [--backend URL]         Pin the backend's content keys (root)
`)
	os.Exit(2)
}

func loadTrustCLI() *trust.Store {
	s, err := trust.LoadForWrite(trust.DefaultPath)
	if err != nil {
		fail("cannot read the trust store: %v", err)
	}
	return s
}

func trustShow() {
	s := loadTrustCLI()
	fmt.Printf("Trust store: %s\n\n", trust.DefaultPath)

	fmt.Println("Content keys (may sign content only; they belong to the LensBridge backend):")
	if len(s.Content) == 0 {
		fmt.Println("  none. This board cannot verify any content package until one is added.")
		fmt.Println("  While online, run: sudo musallahboard-agent trust fetch")
	}
	for _, k := range s.Content {
		printTrustKey(k.KeyID, k.PublicKey, k.Note)
	}

	fmt.Println()
	fmt.Println("Release keys (may sign the board app and the agent):")
	builtin, err := trust.Builtin()
	if err != nil {
		warnf("the release keys compiled into this agent are broken: %v", err)
	}
	for _, pub := range builtin {
		printTrustKey(trust.KeyID(pub), base64.StdEncoding.EncodeToString(pub), "compiled into this agent")
	}
	for _, k := range s.Release {
		printTrustKey(k.KeyID, k.PublicKey, k.Note)
	}
	if len(builtin) == 0 && len(s.Release) == 0 {
		fmt.Println("  none. This is a development build with no release key compiled in, so board app")
		fmt.Println("  and agent updates are refused until one is added with `trust add release`.")
	}
}

func printTrustKey(id, pub, note string) {
	if note != "" {
		note = "  (" + note + ")"
	}
	fmt.Printf("  %s  %s%s\n", id, pub, note)
}

func trustAdd(roleArg, b64, note string) {
	role, err := trust.ParseRole(roleArg)
	if err != nil {
		fail("%v", err)
	}
	pub, err := trust.ParsePublicKey(b64)
	if err != nil {
		fail("%v", err)
	}
	s := loadTrustCLI()
	if !s.Add(role, pub, note) {
		fmt.Printf("Key %s is already trusted; nothing changed.\n", trust.KeyID(pub))
		return
	}
	if err := s.Save(trust.DefaultPath); err != nil {
		fail("could not write %s: %v", trust.DefaultPath, err)
	}
	fmt.Printf("Now trusting %s key %s.\n", role, trust.KeyID(pub))
	if role == trust.RoleRelease {
		fmt.Println("Packages signed by it can install software on this board. Only add keys you are sure of.")
	}
}

func trustRemove(keyID string) {
	s := loadTrustCLI()
	if !s.Remove(keyID) {
		builtin, _ := trust.Builtin()
		for _, pub := range builtin {
			if trust.KeyID(pub) == keyID {
				fail("key %s is compiled into this agent and cannot be removed from trust.json", keyID)
			}
		}
		fail("no key %s in %s (see `musallahboard-agent trust show`)", keyID, trust.DefaultPath)
	}
	if err := s.Save(trust.DefaultPath); err != nil {
		fail("could not write %s: %v", trust.DefaultPath, err)
	}
	fmt.Printf("Removed key %s.\n", keyID)
	if len(s.Content) == 0 {
		warnf("no content key is left, so this board will refuse all content until one is added\n         (sudo musallahboard-agent trust fetch).")
	}
}

func trustFetch(args []string) {
	fs := flag.NewFlagSet("trust fetch", flag.ExitOnError)
	backend := fs.String("backend", "", "backend base URL, for a board that is not enrolled (default: backend_url from "+defaultConfigPath+")")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		trustUsage()
	}
	requireRoot("trust fetch")
	url := *backend
	if url == "" {
		cfg, err := config.Load(defaultConfigPath)
		if err != nil {
			fail("cannot tell which backend to ask: %v\n       Pass it explicitly: sudo musallahboard-agent trust fetch --backend https://<backend>", err)
		}
		url = cfg.BackendURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fmt.Printf("Asking %s for its content signing keys ...\n", strings.TrimRight(url, "/"))
	keys, err := enroll.FetchSigningKeys(ctx, url, nil)
	if err != nil {
		fail("%v\n       Nothing has changed. Check the board's internet connection and try again.", err)
	}
	if len(keys) == 0 {
		fail("the backend has no content signing key configured yet, so there is nothing to pin.\n       Nothing has changed.")
	}
	added, err := enroll.PinContentKeys(trust.DefaultPath, keys, "lensbridge (fetched "+time.Now().UTC().Format("2006-01-02")+")")
	if err != nil {
		fail("%v\n       Nothing has changed.", err)
	}
	if len(added) == 0 {
		fmt.Printf("All %d key(s) the backend publishes are already trusted; nothing changed.\n", len(keys))
		return
	}
	for _, id := range added {
		fmt.Printf("Now trusting content key %s.\n", id)
	}
	if n := len(keys) - len(added); n > 0 {
		fmt.Printf("%d other key(s) were already trusted.\n", n)
	}
	fmt.Println("The board uses them for its next import; no restart is needed.")
}
