// Command bridge mirrors code review between Gerrit and GitHub.
package main

import (
	"flag"
	"log"
)

func main() {
	dryRun := flag.Bool("dry_run", true, "Log intended actions instead of performing them.")
	flag.Parse()
	log.Printf("gerrit-github-bridge starting (dry_run=%v)", *dryRun)
}
