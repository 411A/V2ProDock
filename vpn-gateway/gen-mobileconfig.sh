#!/bin/sh
# Generates /config/vpn/apple.mobileconfig so iOS/tvOS/macOS can install
# the CA + IKEv2 EAP profile in one tap. Re-runnable.
set -eu
PERSIST_DIR="${PERSIST_DIR:-/config/vpn}"
VPN_DOMAIN="${VPN_DOMAIN:-vpn.local}"
VPN_USER_SHOW="${VPN_USER_SHOW:-${VPN_USER:-vpnuser}}"
OUT="${OUT:-$PERSIST_DIR/apple.mobileconfig}"

[ -f "$PERSIST_DIR/ca.crt" ] || { echo "CA not ready yet ($PERSIST_DIR/ca.crt missing)" >&2; exit 1; }
# Portable base64 without line wraps: openssl handles both GNU/BSD.
CA_B64="$(openssl base64 -A -in "$PERSIST_DIR/ca.crt")"
UUID1="$(cat /proc/sys/kernel/random/uuid 2>/dev/null || echo 11111111-1111-1111-1111-111111111111)"
UUID2="$(cat /proc/sys/kernel/random/uuid 2>/dev/null || echo 22222222-2222-2222-2222-222222222222)"
UUID3="$(cat /proc/sys/kernel/random/uuid 2>/dev/null || echo 33333333-3333-3333-3333-333333333333)"

cat > "$OUT" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadDisplayName</key><string>V2ProDock IKEv2 ($VPN_DOMAIN)</string>
  <key>PayloadIdentifier</key><string>com.v2prodock.ikev2.$UUID1</string>
  <key>PayloadUUID</key><string>$UUID1</string>
  <key>PayloadType</key><string>Configuration</string>
  <key>PayloadVersion</key><integer>1</integer>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadType</key><string>com.apple.security.root</string>
      <key>PayloadIdentifier</key><string>com.v2prodock.ikev2.ca.$UUID2</string>
      <key>PayloadUUID</key><string>$UUID2</string>
      <key>PayloadVersion</key><integer>1</integer>
      <key>PayloadDisplayName</key><string>V2ProDock VPN CA</string>
      <key>PayloadContent</key><data>$CA_B64</data>
    </dict>
    <dict>
      <key>PayloadType</key><string>com.apple.vpn.managed</string>
      <key>PayloadIdentifier</key><string>com.v2prodock.ikev2.vpn.$UUID3</string>
      <key>PayloadUUID</key><string>$UUID3</string>
      <key>PayloadVersion</key><integer>1</integer>
      <key>PayloadDisplayName</key><string>V2ProDock IKEv2</string>
      <key>UserDefinedName</key><string>V2ProDock IKEv2 ($VPN_DOMAIN)</string>
      <key>VPNType</key><string>IKEv2</string>
      <key>IKEv2</key>
      <dict>
        <key>RemoteAddress</key><string>$VPN_DOMAIN</string>
        <key>RemoteIdentifier</key><string>$VPN_DOMAIN</string>
        <key>LocalIdentifier</key><string>$VPN_USER_SHOW</string>
        <key>AuthName</key><string>$VPN_USER_SHOW</string>
        <key>AuthPassword</key><string>***-enter-at-connect-***</string>
        <key>ExtendedAuthEnabled</key><integer>1</integer>
        <key>AuthMethod</key><string>None</string>
        <key>ChildSecurityAssociationParameters</key>
        <dict><key>EncryptionAlgorithm</key><string>AES-256</string></dict>
        <key>IKEv2SecurityAssociationParameters</key>
        <dict><key>EncryptionAlgorithm</key><string>AES-256</string></dict>
        <key>OnDemandEnabled</key><integer>0</integer>
      </dict>
    </dict>
  </array>
</dict>
</plist>
EOF
echo "Wrote $OUT (install on device, then enter VPN password at connect)."
