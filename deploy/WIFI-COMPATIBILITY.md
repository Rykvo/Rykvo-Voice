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
| Provisioned IMS identities | EC20 ISIM IMPI/IMPU/domain reading implemented in v1.5.3 | Card-bound atomic set, strict validation, bounded reads, cleanup and identity recheck; no-ISIM retains USIM derivation; native-QMI-only identity reading remains pending |
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

## Phone developer documentation audit / v1.5.3

Sources reviewed 2026-09-25 (primary documentation, not operator-name guesses):

- Android carrier configuration and SIM selectors: https://source.android.com/docs/core/connect/carrier
- Android IWLAN configuration reference: https://developer.android.com/reference/android/telephony/CarrierConfigManager.Iwlan
- Android IMS implementation architecture: https://source.android.com/docs/core/connect/ims
- Android TS.43 provisioning integration: https://source.android.com/docs/core/connect/ims-service-entitlement
- ISIM EF definitions, TS 31.103 sections 4.2.2–4.2.4: https://www.etsi.org/deliver/etsi_ts/131100_131199/131103/18.03.00_60/ts_131103v180300p.pdf
- Apple carrier settings updates: https://support.apple.com/en-ie/109324

| Operator-dependent input | Selection/negotiation | Rykvo implementation / remaining work |
|---|---|---|
| MNO/MVNO service profile | Home PLMN, IMSI, GID, SPN and applicable SIM selectors | Existing 655 rules; rules are data, not 655 verified networks |
| ePDG endpoint | Operator override, home/visited network, address family and discovery priority | Current override + standard derivation; full Android IWLAN policy parity is not claimed |
| IKE/IPsec | Supported proposals, identity, NAT/DPD/rekey, authenticated responder | Existing negotiation and recovery; do not disable authentication to force compatibility |
| IMS registration identity | ISIM-provisioned IMPI/IMPU/domain, or USIM-derived identity when no ISIM exists | Added complete ISIM set on EC20; explicit local overrides remain higher priority |
| SIM authentication application | ePDG/EAP and IMS can use different UICC applications | ISIM for provisioned IMS identity; default EAP application is preserved |
| IMS transport and registration headers | P-CSCF result, transport/security, expiry and operator header policy | Existing carrier profile + negotiation; 423 expiry handling retained |
| Service activation | Operator-specific entitlement endpoint and optional subscriber activation UI | General TS.43 client integration remains missing; an enabled UI flag does not provision service |
| Access location/emergency address | Actual access location and operator activation flow | Home MCC is not physical location; automatic location/entitlement integration remains missing |
| Media, messaging, roaming policy | Codec negotiation and separate service/network acceptance | Registration alone does not prove calls/audio, SMS, MMS or roaming support |

The public phone documents describe mechanisms and configuration keys; they do
not provide every carrier's current private provisioning policy. The target is
automatic SIM-based selection plus standards negotiation, with new carrier data
added independently of module indices. Treat a carrier as accepted only after
its actual service tests, not because a catalogue entry or switch exists.

ISIM implementation reads EF_IMPI (6F02), EF_DOMAIN (6F03) and EF_IMPU (6F04),
uses FCP lengths and bounded READ operations, validates TLVs before use, keeps
all identities in session memory and never emits them to the UI or logs. It
restores a touched basic channel or closes its logical channel even on failure.
Changed cards, failed cleanup and incomplete records stop before registration.
Currently accepted identity syntax is ASCII NAI and SIP/SIPS identity with a DNS
domain; other provisioned syntax is rejected rather than silently rewritten.

Candidate test on 3HK 05 at 2026-09-25 13:51 UTC: SIM/application discovery
completed, no ISIM was present, USIM-derived registration reached initial SIP
403. The new generic ISIM path therefore does not fix that card's rejection.
No subscriber setting was switched off and no cellular attach was requested.

### Hardware correction / v1.5.4

v1.5.3 passed simulated tests but hardware acceptance found empty IMPU spare
records encoded as `80 00` followed by FF. Treating those optional slots as a
malformed mandatory identity prevented the existing US cards from registering.
The host and Latest were rolled back to v1.5.2; v1.5.3 was marked prerelease.
v1.5.4 skips only well-formed empty IMPU slots, still rejects empty IMPI/domain
or an entirely empty IMPU set, and adds regression coverage. On 2026-09-25 at
14:06:14 UTC, module 16 registered with `ims_identity_source=isim` and reached
`sms_ready` using the corrected candidate. This verifies provisioned identity
selection on a live card, not a real SMS delivery or voice/media test.


### Renewal recovery / v1.5.6

At 2026-09-25 15:02:29 UTC, the three previously registered sessions failed
at their first 48-minute refresh. A diagnostic-only early refresh reproduced
a valid SIP 401 on the existing protected session. The provider explicitly
rejected all protected re-challenges, while the bridge treated the failure
as non-retryable. Hardware remained online and Wi-Fi intent remained enabled.

TS 24.229 section 5.1.1.5.1 describes network-requested re-authentication:
https://www.etsi.org/deliver/etsi_ts/124200_124299/124229/14.21.00_60/ts_124229v142100p.pdf

The correction emits a typed recovery signal only for a syntactically valid
AKA challenge on an already registered, protected, positive-expiry runtime
REGISTER. The lifecycle revokes readiness, cleans IMS/tunnel resources, then
rebuilds using the existing delayed retry coordinator and unchanged radio
checkpoint. Initial registration still performs all normal AKA/security checks.
403, malformed challenges, initial authentication failures and cleanup failures
are not recast as recoverable renewal requests.

This is fresh-session recovery, NOT seamless overlapping IPsec-SA rotation.
There can be a short service interruption at re-authentication. An accelerated
renewal probe is distinct from a full natural-duration soak test and from
voice/SMS/MMS acceptance. No module index or carrier-specific allowlist selects
this recovery behavior.

Candidate acceptance: diagnostic-only 35-second renewals on module 03 reached
three successive protected 401 challenges at 22:41:01, 22:41:43 and 22:42:25 UTC
on 2026-09-25. Each rebuilt and registered after about six seconds; radio-off
and user intent remained confirmed. The accelerated timer is not in release
code. The worker IPC allowlist explicitly carries the fixed, non-sensitive
reauthentication diagnostic and rejects suffixes containing private text.
v1.5.5 publication was cancelled before release to include this IPC correction.
