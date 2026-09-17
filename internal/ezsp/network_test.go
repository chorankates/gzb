package ezsp

import (
	"bytes"
	"testing"
)

// joinNetwork takes a node type and the 20-byte EmberNetworkParameters, 21
// bytes in all; formNetwork takes the same 20 without the role. The two
// encoders share networkParams so the layouts cannot drift apart.
func TestJoinParamsLayout(t *testing.T) {
	ext := EUI64{0x4B, 0xF9, 0x3D, 0xC3, 0x75, 0x45, 0x69, 0x2B}
	got := joinParams(JoinConfig{
		Channel:       15,
		PanID:         0x6F88,
		ExtendedPanID: ext,
		NwkUpdateID:   7,
		TxPower:       8,
		NodeType:      NodeRouter,
	})
	want := []byte{
		0x02,                                           // router
		0x2B, 0x69, 0x45, 0x75, 0xC3, 0x3D, 0xF9, 0x4B, // extended PAN ID, LSB first
		0x88, 0x6F, // PAN ID
		0x08,       // tx power
		0x0F,       // channel
		0x00,       // MAC association
		0x00, 0x00, // network manager
		0x07,                   // nwkUpdateId, from the beacon
		0x00, 0x80, 0x00, 0x00, // channel mask: just channel 15
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("joinParams =\n% X\nwant\n% X", got, want)
	}
}

// A joiner must not claim to hold a network key it does not have, and must
// insist the one it is given arrives encrypted; a coordinator holds both keys.
func TestSecurityStateBitmasks(t *testing.T) {
	if joinerSecurity&secHaveNetworkKey != 0 {
		t.Error("a joiner must not set HAVE_NETWORK_KEY: the trust centre supplies it")
	}
	for _, bit := range []uint16{secHavePreconfiguredKey, secRequireEncryptedKey, secTrustCenterGlobalLinkKey} {
		if joinerSecurity&bit == 0 {
			t.Errorf("joiner bitmask 0x%04X lacks 0x%04X", joinerSecurity, bit)
		}
		if coordinatorSecurity&bit == 0 {
			t.Errorf("coordinator bitmask 0x%04X lacks 0x%04X", coordinatorSecurity, bit)
		}
	}
	if coordinatorSecurity&secHaveNetworkKey == 0 {
		t.Error("a coordinator must set HAVE_NETWORK_KEY")
	}

	// EmberInitialSecurityState is 43 bytes: bitmask, two keys, a sequence
	// number and the trust centre's address.
	params := securityStateParams(joinerSecurity, [16]byte{})
	if len(params) != 43 {
		t.Fatalf("securityStateParams is %d bytes, want 43", len(params))
	}
	if !bytes.Equal(params[2:18], ZigbeeAlliance09Key[:]) {
		t.Error("the preconfigured key must be the well-known ZigBeeAlliance09 key")
	}
}

// The join outcomes are the statuses a stackStatusHandler carries when a
// join ends some way other than up. Their values follow EmberStatus in
// ember-types.h; getting one wrong would turn a named failure into
// "status 0xAB".
func TestJoinFailureStatusesAreNamed(t *testing.T) {
	for _, s := range []EmberStatus{
		StatusJoinFailed, StatusNoBeacons, StatusNoNetworkKeyReceived,
		StatusNoLinkKeyReceived, StatusPreconfiguredKeyRequired, StatusSecurityStateNotSet,
	} {
		if got := s.String(); len(got) == 0 || got[:6] == "status" {
			t.Errorf("EmberStatus 0x%02X renders as %q, want a name", uint8(s), got)
		}
	}
	if StatusNoBeacons != 0xAB || StatusJoinFailed != 0x94 || StatusNoNetworkKeyReceived != 0xAD {
		t.Error("join failure status values do not match EmberStatus")
	}
}
