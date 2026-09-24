// Command mbpack builds, signs, verifies and inspects MusallahBoard update
// packages (.mbu, docs/architecture.md section 4).
//
//	mbpack keygen
//	mbpack app   --version 2.1.0 --local-api 2 (--key-file K | --key-env NAME)... <dist dir> -o out.mbu
//	mbpack agent --version 0.3.0 --arch arm64  (--key-file K | --key-env NAME)... <binary>   -o out.mbu
//	mbpack verify [--content-key B64]... [--release-key B64]... <file.mbu>
//	mbpack inspect <file.mbu>
//
// Content packages are built by the LensBridge backend, not here.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	_ "time/tzdata" // content timezones must resolve on any OS

	"github.com/LensBridge/agent/internal/mbu"
	"github.com/LensBridge/agent/internal/trust"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "app":
		err = buildApp(os.Args[2:])
	case "agent":
		err = buildAgent(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "inspect":
		err = inspect(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mbpack:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  mbpack keygen
  mbpack app   --version X.Y.Z --local-api N (--key-file F | --key-env NAME)... <dist dir> -o out.mbu
  mbpack agent --version X.Y.Z --arch arm64|amd64 (--key-file F | --key-env NAME)... <binary> -o out.mbu
  mbpack verify [--content-key B64]... [--release-key B64]... <file.mbu>
  mbpack inspect <file.mbu>
`)
	os.Exit(2)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func keygen() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Printf("private seed (keep secret): %s\n", base64.StdEncoding.EncodeToString(priv.Seed()))
	fmt.Printf("public key:                 %s\n", base64.StdEncoding.EncodeToString(pub))
	fmt.Printf("key id:                     %s\n", trust.KeyID(pub))
	return nil
}

// signingFlags registers --key-file and --key-env and returns a loader.
func signingFlags(fs *flag.FlagSet) func() ([]ed25519.PrivateKey, error) {
	var files, envs multi
	fs.Var(&files, "key-file", "file holding a base64 Ed25519 seed (repeatable)")
	fs.Var(&envs, "key-env", "environment variable holding a base64 Ed25519 seed (repeatable)")
	return func() ([]ed25519.PrivateKey, error) {
		var keys []ed25519.PrivateKey
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			k, err := trust.ParsePrivateSeed(string(raw))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			keys = append(keys, k)
		}
		for _, e := range envs {
			v := os.Getenv(e)
			if v == "" {
				return nil, fmt.Errorf("environment variable %s is empty", e)
			}
			k, err := trust.ParsePrivateSeed(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e, err)
			}
			keys = append(keys, k)
		}
		if len(keys) == 0 {
			return nil, errors.New("no signing key: pass --key-file or --key-env")
		}
		return keys, nil
	}
}

// parseArgs lets flags follow the positional argument.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos, nil
}

func buildApp(args []string) error {
	fs := flag.NewFlagSet("app", flag.ExitOnError)
	version := fs.String("version", "", "app version, MAJOR.MINOR.PATCH")
	localAPI := fs.Int("local-api", 2, "local kiosk API version the build needs")
	out := fs.String("o", "", "output file")
	keys := signingFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 || *out == "" {
		usage()
	}
	signers, err := keys()
	if err != nil {
		return err
	}
	root := pos[0]
	var src []mbu.Source
	err = filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		src = append(src, mbu.Source{Path: filepath.ToSlash(rel), FromFile: p})
		return nil
	})
	if err != nil {
		return err
	}
	m := mbu.Manifest{Type: mbu.TypeApp, Version: *version, App: &mbu.AppInfo{LocalAPI: *localAPI}}
	return writePackage(*out, m, src, signers)
}

func buildAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	version := fs.String("version", "", "agent version, MAJOR.MINOR.PATCH")
	arch := fs.String("arch", "arm64", "arm64 or amd64")
	out := fs.String("o", "", "output file")
	keys := signingFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 || *out == "" {
		usage()
	}
	signers, err := keys()
	if err != nil {
		return err
	}
	const name = "musallahboard-agent"
	m := mbu.Manifest{Type: mbu.TypeAgent, Version: *version, Agent: &mbu.AgentInfo{Arch: *arch, Binary: name}}
	return writePackage(*out, m, []mbu.Source{{Path: name, FromFile: pos[0]}}, signers)
}

func writePackage(out string, m mbu.Manifest, src []mbu.Source, signers []ed25519.PrivateKey) error {
	f, err := os.Create(out + ".part")
	if err != nil {
		return err
	}
	if err := mbu.Build(f, m, src, signers); err != nil {
		f.Close()
		os.Remove(out + ".part")
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(out+".part", out); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%s, %d files)\n", out, m.Type, len(src))
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var ck, rk multi
	fs.Var(&ck, "content-key", "trusted content public key, base64 (repeatable)")
	fs.Var(&rk, "release-key", "trusted release public key, base64 (repeatable)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 1 {
		usage()
	}
	parse := func(list []string) ([]ed25519.PublicKey, error) {
		var out []ed25519.PublicKey
		for _, b := range list {
			k, err := trust.ParsePublicKey(b)
			if err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, nil
	}
	c, err := parse(ck)
	if err != nil {
		return err
	}
	r, err := parse(rk)
	if err != nil {
		return err
	}
	builtin, err := trust.Builtin()
	if err != nil {
		return err
	}
	pkg, err := mbu.Open(pos[0], trust.NewRing(c, append(r, builtin...)), mbu.OpenOptions{})
	if err != nil {
		return err
	}
	defer pkg.Close()
	for _, f := range pkg.Manifest.Files {
		if err := pkg.CopyFile(f.Path, discard{}); err != nil {
			return err
		}
	}
	fmt.Printf("OK: %s, signed by %s, %d files verified\n", pkg.Manifest.Describe(), pkg.SignedBy, len(pkg.Manifest.Files))
	return nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func inspect(args []string) error {
	if len(args) != 1 {
		usage()
	}
	m, err := mbu.Peek(args[0])
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println("(unverified)")
	fmt.Println(string(out))
	return nil
}
