package ims

import (
	"testing"

	"rykvo.local/auth/internal/vocat/vowifi"
)

func TestProvisionedIMSIdentityOverridesCarrierDefaults(t *testing.T) {
	for _, plmn := range [][2]string{{"454", "03"}, {"310", "280"}, {"234", "10"}, {"001", "01"}} {
		id := vowifi.SIMIdentity{IMSI: plmn[0] + plmn[1] + "0000000001", HomeMCC: plmn[0], HomeMNC: plmn[1], ProvisionedIMS: &vowifi.ProvisionedIMSIdentity{
			PrivateIdentity: "subscriber@private.example", Domain: "registrar.example", PublicIdentities: []string{"sip:+12345678901@public.example", "sip:alias@public.example"},
		}}
		got, err := deriveIdentities(id, Config{})
		if err != nil || got.domain != "registrar.example" || got.private != "subscriber@private.example" || got.public != "sip:+12345678901@public.example" || got.user != "+12345678901" || got.temporaryPublic {
			t.Fatalf("ISIM not used for %v: %v", plmn, err)
		}
		got, err = deriveIdentities(id, Config{PrivateIdentity: "manual@private.example", PublicIdentity: "sip:manual@public.example"})
		if err != nil || got.private != "manual@private.example" || got.public != "sip:manual@public.example" || got.temporaryPublic {
			t.Fatal("explicit configuration not preserved")
		}
		id.ProvisionedIMS.PrivateIdentity = "invalid\r\nAuthorization: x"
		if _, err := deriveIdentities(id, Config{}); err == nil {
			t.Fatal("malformed ISIM was used")
		}
	}
}

func TestNoISIMRetainsTemporaryIdentity(t *testing.T) {
	got, err := deriveIdentities(vowifi.SIMIdentity{IMSI: "001010000000001", HomeMCC: "001", HomeMNC: "01"}, Config{})
	if err != nil || !got.temporaryPublic || got.public != "sip:001010000000001@ims.mnc001.mcc001.3gppnetwork.org" {
		t.Fatal("USIM fallback changed")
	}
}
