//go:build !linux

package opencodeacp

// nativeOwnedHomeRefusal is why every non-Linux platform refuses a durable
// native home owned by someone other than the isolated identity: there is no
// ownership model to consult, so the home is refused outright.
const nativeOwnedHomeRefusal = "ownership validation is unsupported"
