package main

import (
	app "github.com/signbyte/envelope"

	"azugo.io/azugo/server"
	"azugo.io/core/cli"
)

func init() {
	cli.Register(server.HealthCommand("/healthz", server.Options{
		AppName:       "Envelope/Workflow service (envelope)",
		AppVer:        Version,
		Configuration: app.NewConfiguration(),
	}))
}
