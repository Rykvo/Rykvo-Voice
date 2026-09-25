# Wi-Fi Calling compatibility and acceptance

## Normative baseline

- GSMA IR.51 (UE/network IMS profile): https://www.gsma.com/newsroom/gsma_resources/ir-51-ims-over-wi-fi-v/
- 3GPP TS 24.302 (non-3GPP EPC access): https://portal.3gpp.org/desktopmodules/Specifications/SpecificationDetails.aspx?specificationId=1073
- 3GPP TS 24.229 (IMS registration and SIP): https://www.etsi.org/deliver/etsi_ts/124200_124299/124229/16.15.00_60/ts_124229v161500p.pdf
- RFC 3261 sections 10.2.8, 20.23 (423 / Min-Expires): https://www.rfc-editor.org/rfc/rfc3261.html#section-10.2.8
- AOSP carrier selection: https://source.android.com/docs/core/connect/carrier
- AOSP entitlement: https://source.android.com/docs/core/connect/ims-service-entitlement

These documents define shared behavior, not a universal operator activation file.
A catalogue match is not proof of registration, SMS, voice/media or MMS support.

## Implementation and gaps

| Stage | Current status | Acceptance |
|---|---|---|
| SIM and carrier selection | Home PLMN + available GID/SPN/ICCID rules | No matching by module index, phone prefix or visited operator |
| ePDG | Carrier override and standard selection | DNS + verified authenticated IKE/IPsec, not hostname match alone |
| IMS REGISTER | AKA, TCP/UDP and P-CSCF handling | Correlated 200 response, contact expiry and required security |
| 423 negotiation | Added bounded Min-Expires retry | One increasing retry; strict header validation; maximum 24 hours; remembered for refresh; unregister remains Expires: 0 |
| Provisioned IMS identities | Generic identities and selected legacy exceptions | Generic ISIM IMPI/IMPU/domain discovery still requires implementation |
| Entitlement | No general TS.43 integration | Per-operator entitlement server/configuration and activation tests needed |
| PANI location | Existing AUTO uses home MCC | This is not measured access location; do not equate it with iPhone location behavior |
| Runtime recovery | Single retry coordinator, bounded backoff | Verify renewal, internet loss, reboot and explicit off per operator |
| Voice/media and MMS | Not accepted end-to-end by these changes | Actual incoming/outgoing calls, audio and MMS required separately |

## 2026-09-25 observations

The user verified 3HK on iPhone in airplane mode on the same router, including a
successful call. The host therefore still has an interoperability gap.

The full mirrored Hutchison_hk.bundle directory (7 files) was audited at commit
9bb6e6926fed05c4434a44c17da5bfff98715ad2 of dwilliamsuk/ios-carrier-bundles.
This is a mirror, not an official current signed IPCC or the iOS telephony engine.
The portable importer reports ignored/nonportable fields instead of executing them.

A temporary 3HK PANI AUTO trial advanced to SIP 423. The new standard Min-Expires
retry was tested against the live card and then received an initial-stage SIP 403.
It has not established IMS registration. The PANI trial was removed from the
production carrier catalogue: it neither proves interoperability nor determines
actual location. No certificate or authentication checks were disabled.

## Number presentation

AT+CNUM international type-of-address (TON=international, NPI=ISDN) preserves or
adds the international marker. Unknown/national type remains unchanged. An IMS
number stored against the current ICCID may normalize a bare number only when
all digits match exactly. Existing explicit international numbers are not replaced.
A bare 133 prefix never implies China. National formatting requires an explicit
region hint; international formatting uses the explicit country calling code.

## Required per-carrier matrix

Record separate evidence for SIM selection, tunnel, IMS registration, renewal,
SMS send/receive, calls/audio, and MMS. Repeat after reboot and network interruption.
Keep Wi-Fi intent/radio-off policy and manual-off behavior. Unknown and untested
carriers remain unverified rather than being reported as supported automatically.
