//go:build linux

package opencode

func handoffSessionCarrierGeneratedTree(root string, isolation *ProcessIsolation) error {
	return handoffGeneratedNativeTree(root, isolation)
}
