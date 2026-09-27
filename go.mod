module github.com/jlbyh2o/ezdr

go 1.27

// The web UI's node_modules may contain stray .go files from npm packages.
ignore ./web/node_modules

require (
	connectrpc.com/connect v1.21.0
	google.golang.org/protobuf v1.36.12
)
