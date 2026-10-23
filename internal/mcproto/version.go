package mcproto

import "slices"

// SupportedVersions describes the stable Java protocol envelopes checked here.
// It does not imply translation between different client and server versions.
const SupportedVersions = "Minecraft Java 1.7.10 through 26.1.2 (stable releases)"

// Primary schema: PrismarineJS/minecraft-data at
// 1df9432c07c81b47326e4fb1e06d3c8d723bf4aa, data/pc/common/protocolVersions.json
// and data/pc/{1.7,1.18.2,1.19,1.19.2,1.19.3,1.20.2,1.20.5,26.1}/protocol.json.
// Keep this finite: unknown snapshots/future versions must not silently inherit
// a login layout. A new stable version needs a schema check and a boundary test.
var supportedProtocols = [...]int{
	5, 47, 107, 108, 109, 110, 210, 315, 316, 335, 338, 340,
	393, 401, 404, 477, 480, 485, 490, 498, 573, 575, 578,
	735, 736, 751, 753, 754, 755, 756, 757, 758,
	759, 760, 761, 762, 763, 764, 765, 766, 767, 768, 769,
	770, 771, 772, 773, 774, 775,
}

func SupportedProtocol(protocol int) bool {
	return slices.Contains(supportedProtocols[:], protocol)
}

// SupportedProtocols returns a private copy for configuration/schema tooling.
func SupportedProtocols() []int {
	return append([]int(nil), supportedProtocols[:]...)
}

type loginLayout uint8

const (
	loginNameOnly loginLayout = iota
	loginSigned
	loginSignedUUID
	loginOptionalUUID
	loginUUID
)

type versionProfile struct {
	login    loginLayout
	transfer bool
}

func profileFor(protocol int) (versionProfile, bool) {
	if !SupportedProtocol(protocol) {
		return versionProfile{}, false
	}
	profile := versionProfile{transfer: protocol >= 766}
	switch {
	case protocol <= 758:
		profile.login = loginNameOnly
	case protocol == 759:
		profile.login = loginSigned
	case protocol == 760:
		profile.login = loginSignedUUID
	case protocol <= 763:
		profile.login = loginOptionalUUID
	default:
		profile.login = loginUUID
	}
	return profile, true
}

func validNextState(protocol, state int) bool {
	if state == 1 || state == 2 {
		return true
	}
	profile, supported := profileFor(protocol)
	return state == 3 && supported && profile.transfer
}
