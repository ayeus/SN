package platform

// Version is the release this binary was built from. It is set at build time
// from the VERSION file at the root of the repository:
//
//	go build -ldflags "-X github.com/ayeus/ayeusann/internal/platform.Version=$(cat VERSION)"
//
// A binary built without that flag (`go run`, a test) reports "dev".
var Version = "dev"
