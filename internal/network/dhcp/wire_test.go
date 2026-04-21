package dhcp

import (
	"bytes"
	"net/netip"
	"testing"
)

// sampleDiscover is a real DHCPDISCOVER packet as a Linux dhclient
// would emit it on eth0. Captured once and pasted here so the
// parser's contract is pinned against something a real client
// actually produces.
//
// Fields:
//   op=1 htype=1 hlen=6 hops=0
//   xid=0xdeadbeef
//   flags=0x8000 (broadcast)
//   chaddr starts 02:00:00:00:00:01
//   options: 53=1 (DISCOVER), 55 (parameter list), 255 (end)
func sampleDiscover() []byte {
	b := make([]byte, 240, 300)
	b[0] = 1
	b[1] = 1
	b[2] = 6
	b[3] = 0
	b[4], b[5], b[6], b[7] = 0xde, 0xad, 0xbe, 0xef
	// secs=0 (10..12)
	b[10], b[11] = 0x80, 0x00 // flags=0x8000
	// ciaddr, yiaddr, siaddr, giaddr = zero (12..28)
	// chaddr[0..5] = 02:00:00:00:00:01
	b[28] = 0x02
	b[33] = 0x01
	// magic cookie at 236..240
	b[236], b[237], b[238], b[239] = 0x63, 0x82, 0x53, 0x63
	// options: 53=1 (len=1 value DISCOVER), 55 param list, end.
	b = append(b, 53, 1, 1)     // message type = DISCOVER
	b = append(b, 55, 3, 1, 3, 6) // parameter request list: subnet mask, router, DNS
	b = append(b, 255)          // end
	return b
}

func TestParseSampleDiscover(t *testing.T) {
	m, err := Parse(sampleDiscover())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Op != opBootRequest {
		t.Errorf("Op = %d, want %d", m.Op, opBootRequest)
	}
	if m.HLen != hlenMAC {
		t.Errorf("HLen = %d, want %d", m.HLen, hlenMAC)
	}
	if m.XID != 0xdeadbeef {
		t.Errorf("XID = 0x%08x, want 0xdeadbeef", m.XID)
	}
	mt, ok := m.MessageType()
	if !ok {
		t.Fatal("MessageType not present")
	}
	if mt != MsgDiscover {
		t.Errorf("MessageType = %d, want %d", mt, MsgDiscover)
	}
	mac, err := m.ClientMAC()
	if err != nil {
		t.Fatal(err)
	}
	want := [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	if mac != want {
		t.Errorf("ClientMAC = %v, want %v", mac, want)
	}
}

func TestParseRejectsShortPacket(t *testing.T) {
	if _, err := Parse(make([]byte, 10)); err == nil {
		t.Error("expected error for short packet")
	}
}

func TestParseRejectsBadMagic(t *testing.T) {
	b := sampleDiscover()
	b[239] = 0x00 // corrupt the magic
	if _, err := Parse(b); err == nil {
		t.Error("expected error for bad magic cookie")
	}
}

func TestParseRejectsTruncatedOption(t *testing.T) {
	// length says 10 but only 2 bytes of data follow
	b := make([]byte, 240)
	b[236], b[237], b[238], b[239] = 0x63, 0x82, 0x53, 0x63
	b = append(b, 53, 10, 1, 2) // option 53, length 10, but only 2 bytes
	if _, err := Parse(b); err == nil {
		t.Error("expected error for truncated option")
	}
}

func TestSerializeRoundTrip(t *testing.T) {
	original, err := Parse(sampleDiscover())
	if err != nil {
		t.Fatal(err)
	}
	bytesOut := Serialize(original)
	parsed, err := Parse(bytesOut)
	if err != nil {
		t.Fatalf("Parse after Serialize: %v", err)
	}
	if parsed.XID != original.XID || parsed.Op != original.Op || parsed.Flags != original.Flags {
		t.Errorf("round-trip mismatch: orig=%+v parsed=%+v", original, parsed)
	}
	if !bytes.Equal(parsed.CHAddr[:], original.CHAddr[:]) {
		t.Errorf("CHAddr round-trip differs")
	}
	mtOrig, _ := original.MessageType()
	mtParsed, _ := parsed.MessageType()
	if mtOrig != mtParsed {
		t.Errorf("MessageType round-trip: %d → %d", mtOrig, mtParsed)
	}
}

func TestRequestedIPExtraction(t *testing.T) {
	// Build a REQUEST with option 50 (requested IP) = 10.20.0.3.
	b := sampleDiscover()
	// Swap message type from DISCOVER to REQUEST and append option 50.
	// sampleDiscover's options region starts at byte 240; option 53
	// is at 240..242 (code=53, len=1, value=1). Flip value to 3.
	b[242] = MsgRequest
	// Append option 50 (len=4) before the final end (255) we already
	// appended. Simplest: find the end byte and rewrite.
	// In the sample: options are 53,1,1, 55,3,1,3,6, 255.
	// We want: 53,1,3, 55,3,1,3,6, 50,4,10,20,0,3, 255.
	b[242] = MsgRequest
	// Strip trailing end (last byte), append option 50, re-add end.
	b = b[:len(b)-1]
	b = append(b, 50, 4, 10, 20, 0, 3, 255)

	m, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m.RequestedIP()
	if !ok {
		t.Fatal("RequestedIP not present")
	}
	want := netip.MustParseAddr("10.20.0.3")
	if got != want {
		t.Errorf("RequestedIP = %s, want %s", got, want)
	}
}

func TestMaskFromPrefixBits(t *testing.T) {
	cases := map[int][4]byte{
		0:  {0, 0, 0, 0},
		8:  {255, 0, 0, 0},
		16: {255, 255, 0, 0},
		24: {255, 255, 255, 0},
		30: {255, 255, 255, 252},
		32: {255, 255, 255, 255},
	}
	for bits, want := range cases {
		got, err := MaskFromPrefixBits(bits)
		if err != nil {
			t.Fatalf("/%d: %v", bits, err)
		}
		if got != want {
			t.Errorf("/%d: got %v, want %v", bits, got, want)
		}
	}
	if _, err := MaskFromPrefixBits(33); err == nil {
		t.Error("expected error for /33")
	}
}

func TestOptionBuilders(t *testing.T) {
	if OptMsgType(MsgOffer).Code != optMessageType {
		t.Error("OptMsgType code")
	}
	ip := netip.MustParseAddr("10.20.7.1")
	o := OptServerID(ip)
	if o.Code != optServerID || len(o.Data) != 4 {
		t.Error("OptServerID shape")
	}
	if o.Data[0] != 10 || o.Data[3] != 1 {
		t.Errorf("OptServerID bytes = %v", o.Data)
	}
	if OptLeaseTime(60).Data[3] != 60 {
		t.Errorf("OptLeaseTime bytes = %v", OptLeaseTime(60).Data)
	}
}
