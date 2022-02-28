// +build darwin,!amd64,!arm64

package test

//go:generate gtrace -v

//gtrace:gen
type ConditionalBuildTrace struct {
	OnSomething func()
}
