//go:build !linux

package opencode

func init() {
	supervisorBootstrap()
}
