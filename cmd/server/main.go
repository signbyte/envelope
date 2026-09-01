package main

import (
	"os"

	"azugo.io/core/cli"
)

// Version is set at build time (-ldflags) or left at the dev default.
var Version = "1.0.0-dev"

func main() {
	if _, ok := os.LookupEnv("SERVER_URLS"); !ok {
		_ = os.Setenv("SERVER_URLS", "http://0.0.0.0:8080")
	}

	cli.Run(cli.Options{
		Use:     "server",
		Short:   "Envelope/Workflow service",
		Long:    "Starts the envelope (Envelope/Workflow) web server by default.",
		Version: Version,
	})
}
