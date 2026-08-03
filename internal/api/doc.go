// Package api contains the agent's HTTP client, generated from the backend's
// OpenAPI contract at LensBridgeBackend/openapi.yaml.
//
// Do not edit client.gen.go. To pick up a backend change:
//
//	go generate ./internal/api
//
// The generated file is committed, per Go convention, so a plain `go build`
// needs no code generation step and the diff is reviewable.
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config oapi-codegen.yaml ../../../../LensBridgeBackend/openapi.yaml
package api
