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
- Separate IKE runtime failures and best-effort remote Delete failures from
  local teardown errors. Preserve runtime failure delivery and local cleanup
  interlocks; expose only a sanitized remote-release diagnostic to Rykvo.
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

- Restrict RTP SDP to implemented codecs, reject unsupported clock rates and
  static payload redefinitions, and retain the first audio section address.
  Carrier media additionally supports AMR/AMR-WB via system OpenCORE/VisualOn
  libraries, using RFC 4867 bandwidth-efficient and octet-aligned mono framing.
  Bound frame counts, preserve negotiated mode sets, honor CMR and mode-change
  restrictions, and reject unsupported CRC/interleaving/robust-sorting layouts.
  Convert AMR-WB through a stateful low-pass filter to the existing 8 kHz bridge;
  this is compatibility, not an end-to-end wideband claim.

- Add optional APP-side native Opus at 12 kbit/s mono through system libopus,
  without changing the carrier codec. Keep each call's native state and locks
  independent. Decode compressed RTP in playout order with bounded buffering,
  conceal loss, validate clocks across wrap and release native states on close.
  Missing native libraries never cause unsupported codecs to be advertised.

- Adapt the RTP reserve from 60 to at most 180 ms after repeated late packets.
  Preserve 20 ms playout, sample values, duplicate filtering and bounded memory;
  count rebuffer silence separately from missing samples. Add deterministic
  delay-step, variable-jitter, packet-batch, wrap and reserve-bound regressions.

- Handle CRLF stream keepalives across initial REGISTER, runtime and protected
  inbound TCP. Bound incomplete header lines and serialize runtime writes with
  deadlines. Add fragmented keepalive, body framing and socket regressions.

- Request normal IMS lifecycle recovery after a 60-second unconfirmed call
  teardown; preserve occupancy until verified cleanup. Cancel the watchdog
  on confirmed termination or session shutdown. Export receive counters for
  carrier-side diagnosis without recording audio.

- Conceal short G.711 gaps using bounded periodic history or a short fade;
  fade to silence within 60 ms and crossfade recovery over 5 ms. Do not store
  synthesized audio as source history. Use the same path for reserve growth;
  reduce excess reserve only inside received quiet audio after stable traffic.
  Keep loss counters, per-call state, playout cadence and the 180 ms ceiling.

- Permit negotiated early downlink media without treating it as answered.
  Re-anchor confirmed consecutive forward timestamp jumps and reset receive
  state only on a negotiated endpoint/format change; unchanged SDP retains NAT.
