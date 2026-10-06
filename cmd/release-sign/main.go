// release-sign publishes host-agent releases.
//
//	release-sign keygen -out release.key        create the release key (once)
//	release-sign public -key release.key        print its public half
//	release-sign sign -dir dist/agent -version 0.4.0 [-min-version 0.4.0] -key release.key
//	release-sign verify -dir dist/agent -public <base64>
//
// The private key signs what every enrolled machine will run. Keep it off the
// platform, and keep a copy somewhere safe: without it no update can ever be
// published to machines that are already enrolled.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ayeus/ayeusann/internal/release"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func readKey(path string) string {
	if path == "" {
		if v := os.Getenv("RELEASE_SIGNING_KEY"); v != "" {
			return v
		}
		fail("name the release key with -key FILE (or set RELEASE_SIGNING_KEY)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fail("cannot read the release key: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: release-sign keygen|public|sign|verify (see the top of cmd/release-sign/main.go)")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	keyPath := fs.String("key", "", "file holding the release key")
	out := fs.String("out", "", "where keygen writes the key")
	dir := fs.String("dir", "dist/agent", "directory of agent binaries")
	version := fs.String("version", "", "version being published")
	minVersion := fs.String("min-version", "0.0.0", "oldest agent that is still given work")
	public := fs.String("public", "", "release public key (base64)")
	_ = fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "keygen":
		if *out == "" {
			fail("keygen needs -out FILE")
		}
		if _, err := os.Stat(*out); err == nil {
			fail("%s already exists; it was left alone. Machines that are enrolled trust that key and no other.", *out)
		}
		seed, pub, err := release.GenerateKey()
		if err != nil {
			fail("%v", err)
		}
		if err := os.WriteFile(*out, []byte(seed+"\n"), 0o600); err != nil {
			fail("%v", err)
		}
		fmt.Println(pub)
	case "public":
		key, err := release.PrivateKey(readKey(*keyPath))
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(release.PublicOf(key))
	case "sign":
		if *version == "" {
			fail("sign needs -version")
		}
		key, err := release.PrivateKey(readKey(*keyPath))
		if err != nil {
			fail("%v", err)
		}
		m, err := release.Build(*dir, *version, *minVersion, time.Now())
		if err != nil {
			fail("%v", err)
		}
		if err := release.Sign(*dir, m, key); err != nil {
			fail("%v", err)
		}
		fmt.Printf("signed release %s (minimum %s) in %s\n", m.Version, m.MinVersion, *dir)
		for _, a := range m.Artifacts {
			fmt.Printf("  %-8s %-6s %s  %d bytes\n", a.OS, a.Arch, a.File, a.Size)
		}
	case "verify":
		pub, err := release.PublicKey(*public)
		if err != nil {
			fail("%v", err)
		}
		m, _, _, err := release.Load(*dir, pub)
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("release %s (minimum %s), published %s, %d binaries: signature is good\n",
			m.Version, m.MinVersion, m.PublishedAt.Format(time.RFC3339), len(m.Artifacts))
	default:
		fail("unknown command %q", os.Args[1])
	}
}
