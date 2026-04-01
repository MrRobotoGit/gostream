package main

// AppVersion is the current release version.
// Updated at each release. Overridable at build time via:
//
//	go build -ldflags "-X main.AppVersion=v1.6.4"
var AppVersion = "dev"
