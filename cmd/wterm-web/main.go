// Command wterm-web serves the local tmux server to a browser, and is the CLI
// that enrolls the browsers allowed to reach it.
package main

import "os"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
