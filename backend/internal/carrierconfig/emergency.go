package carrierconfig

import "strings"

// Only verified carrier routes are enabled. Unknown MVNOs never inherit their
// roaming network's account portal or receive a guessed emergency-address URL.
type EmergencyProvider struct {
	ID, Endpoint, PageHost, PagePrefix string
}

var emergencyProviders = []struct {
	provider EmergencyProvider
	selector Profile
}{
	{EmergencyProvider{"att-mvno-us", "https://sentitlement2.mobile.att.net/WFC", "attdashboard.wireless.att.com", "/softphone/primary/"}, Profile{MCC: "310", MNC: "280", MVNOType: "gid", MVNOMatch: "20FF"}},
}

func EmergencyMatch(id Identity) string {
	if !digits(id.MCC, 3, 3) || !digits(id.MNC, 2, 3) || !strings.HasPrefix(id.IMSI, id.MCC+id.MNC) {
		return ""
	}
	for _, route := range emergencyProviders {
		if route.selector.MCC == id.MCC && route.selector.MNC == id.MNC {
			if matched, _ := mvno(route.selector, id); matched {
				return route.provider.ID
			}
		}
	}
	return ""
}

func EmergencyByID(id string) (EmergencyProvider, bool) {
	for _, route := range emergencyProviders {
		if route.provider.ID == id {
			return route.provider, true
		}
	}
	return EmergencyProvider{}, false
}
