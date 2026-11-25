//go:build !linux

package assess

import "context"

func probeOpenNativeAvailable() (bool, string) { return false, "" }

func probeOpenPathNative(context.Context, string) (OpenObservation, bool) {
	return OpenObservation{}, false
}
