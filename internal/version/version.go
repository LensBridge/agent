package version

// Version is the agent build version. Override at build time:
//   go build -ldflags "-X github.com/LensBridge/agent/internal/version.Version=0.1.0"
var Version = "dev"
