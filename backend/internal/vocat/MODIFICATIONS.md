# Selective integration / provenance

Upstream commit and original per-file hashes are recorded in SOURCE.json. The
original Vocat Research & Evaluation License is retained in LICENSE and copied
into release bundles. Importing the subsystem does not change its license terms.

Imported: carrier selection/IPCC importer, orchestrator, AT/native-QMI adapters,
IKE/ESP dataplanes, IMS protocol implementation and runtime, with upstream tests.
App store/UI/auth/automation/update systems and PCSC integration are not imported.
Calls/media remain internal upstream dependencies; Rykvo exposes no call API in
this integration. SMS deliveries are explicitly rejected until persistence exists.

Changes:
- Import paths relocated from vocat/internal to rykvo.local/auth/internal/vocat.
- Extracted modem type declarations and modem-independent SMS PDU codec/types.
- Added the codec's three original error constants and intPointer helper.
- Excluded TestParseCMGLPreservesUndecodableRecord: it tests the non-imported AT
  SMS storage-list layer, not the imported TPDU codec. Other codec tests retained.
- Extracted SIM FCP sizing, CRSM response, SPN and GID codecs from device
  snapshot/phone sources; exported four thin metadata wrappers.
- Rykvo-specific device/lifecycle code lives outside this directory.
- Preserve valid carrier IKE hostnames without requiring an `epdg` label. The
  Hutchison HK profile uses `wlan.three.com.hk`, as specified by
  `TechSettings.IKE.RemoteAddress` in the mirrored Apple carrier bundle:
  https://github.com/dwilliamsuk/ios-carrier-bundles/blob/latest/Carrier%20Bundles/Hutchison_hk.bundle/carrier.plist
  The fix does not import certificate-validation or entitlement bypass settings.

The installed service explicitly selects RYKVO_WIFI_ENGINE=vocat. Empty/legacy
keeps the existing engine for manual binary invocation; there is no automatic
fallback during a session. Linux TUN/XFRM lifecycles have passed isolated tests
with CAP_NET_ADMIN only. Initial registration of two live modules was verified
on deployment; long-term renewal and multi-carrier coverage are separate gates.

The Rykvo bridge uses a private socket-activated session worker with only
CAP_NET_ADMIN. The web/DB process remains unprivileged. Recognized runtime network failures use delayed retries (2/4/8/16/30 seconds,
capped at 30 seconds) while Wi-Fi remains enabled; operator rejections and
unconfirmed local cleanup are not retried. See deploy/VOWIFI-INTEGRATION.md.
No production engine switch is performed merely by importing this code.

- Handle RFC 3261 REGISTER 423 using validated Min-Expires, with one retry per
  transaction and the existing 24-hour expiry ceiling. Preserve the negotiated
  minimum across refreshes and keep deregistration at Expires: 0 even when a
  carrier specifies an expiry override. See deploy/WIFI-COMPATIBILITY.md.

- Add TS 31.103 EC20 ISIM identity discovery/reading, bounded FCP/TLV parsing,
  complete-set validation, UICC cleanup and current-card checks. Prefer these
  identities for IMS; keep EAP's default AKA application separate from ISIM.
  Add logical APDU 6C response-length handling and identity parser fuzz/race tests.
  This does not implement TS.43 provisioning or certify all operators.

- Classify valid protected IMS renewal AKA challenges using a typed lifecycle
  signal; cleanly rebuild authenticated sessions without restoring cellular RF.
  This is recovery, not seamless overlapping SA rotation. Initial authentication
  errors and operator policy refusals remain terminal. Add packet/lifecycle/
  bridge tests and keep diagnostic output free of subscriber credentials.
