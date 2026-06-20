// Command hina is the single multi-command binary for the Hina V2 server:
// `hina server`, `hina setup`, `hina doctor`, `hina migrate`, `hina version`.
package main

import (
	"fmt"
	"os"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "server":
		err = cmdServer(os.Args[2:])
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "migrate":
		err = cmdMigrate(os.Args[2:])
	case "assets":
		err = cmdAssets(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("hina", version)
		return
	case "help", "--help", "-h":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `hina — local voice/text agent (V2)

Usage:
  hina server     Run the server
  hina setup      Create app dirs, run migrations, bootstrap the admin
  hina doctor     Report host capabilities and feature availability
  hina migrate    Apply database migrations (migrate down [N|all] to roll back)
  hina assets     Manage local-inference downloads (status|verify|pull)
  hina version    Print version

Pass -h after a command for its flags.
`)
}
