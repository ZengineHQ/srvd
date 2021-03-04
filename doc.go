// Package srvd provides the ability to deploy new images on a ECS cluster by creating a new task definition based off the currently running on then updating the service to use the new definition.
package main

//go:generate go run -tags codegen srvd.go
//go:generate gofmt -w -s .
