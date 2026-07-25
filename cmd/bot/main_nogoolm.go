//go:build !goolm

// Stub entry point for builds without the goolm tag. The real bot (main.go)
// needs `-tags goolm` for pure-Go end-to-end encryption; without it mautrix
// would require libolm via cgo and would not link at CGO_ENABLED=0. This stub
// keeps `go build ./...` / `go test ./...` working (they still compile and test
// the tag-free config helpers) while making it impossible to accidentally run a
// crypto-less bot.
package main

import "log"

func main() {
	log.Fatal("this bot must be built with -tags goolm (pure-Go E2EE); " +
		"a build without it cannot read or answer in encrypted rooms")
}
