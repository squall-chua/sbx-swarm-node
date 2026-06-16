// Command sbx-swarm-node runs a single Docker-sandbox swarm node.
package main

import "fmt"

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Println("sbx-swarm-node", version)
}
