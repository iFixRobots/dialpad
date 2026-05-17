package main

import (
	"github.com/beeper/dialpad-bridge/pkg/connector"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	m := mxmain.BridgeMain{
		Name:        "dialpad-bridge",
		URL:         "https://github.com/beeper/dialpad-bridge",
		Description: "A Matrix-Dialpad puppeting bridge.",
		Version:     "0.1.0",
		Connector:   connector.NewDialpadConnector(),
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
