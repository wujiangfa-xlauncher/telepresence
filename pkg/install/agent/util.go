package agent

import (
	"strconv"

	"github.com/telepresenceio/telepresence/rpc/v2/manager"
)

// SpecMatchesIntercept answers the question if an InterceptSpec matches the given
// Intercept config. The spec matches if:
//   - its ServiceName is unset or the ServiceName equals the config's ServiceName
//   - its ServicePortIdentifier is unset or:
//      - it can be parsed to an integer equal to the config's ServicePort
//      - it is equal to the config's ServiceName
func SpecMatchesIntercept(spec *manager.InterceptSpec, ic *Intercept) bool {
	if spec.ServiceName != "" && ic.ServiceName != spec.ServiceName {
		return false
	}
	if spec.ServicePortIdentifier != "" {
		if pn, err := strconv.Atoi(spec.ServicePortIdentifier); err == nil {
			if ic.ServicePort != int32(pn) {
				return false
			}
		} else if spec.ServicePortIdentifier != ic.ServicePortName {
			return false
		}
	}
	return true
}
