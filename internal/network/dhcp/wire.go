package dhcp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// Protocol constants from RFC 2131. Named here rather than inlined
// so the code reads against the spec.
const (
	opBootRequest = 1
	opBootReply   = 2

	htypeEthernet = 1
	hlenMAC       = 6

	magicCookie uint32 = 0x63825363

	// Option codes.
	optPad         = 0
	optSubnetMask  = 1
	optRouter      = 3
	optDNSServer   = 6
	optRequestedIP = 50
	optLeaseTime   = 51
	optMessageType = 53
	optServerID    = 54
	optEnd         = 255

	// DHCP message types.
	MsgDiscover = 1
	MsgOffer    = 2
	MsgRequest  = 3
	MsgDecline  = 4
	MsgAck      = 5
	MsgNak      = 6
	MsgRelease  = 7
	MsgInform   = 8

	// Wire-format sizes.
	headerLen    = 236 // op..file
	magicLen     = 4
	minPacketLen = headerLen + magicLen
)

// Message is the parsed form of a DHCP packet. All multi-byte
// header fields are big-endian on the wire; we hold them in Go
// native types. Options are decoded into a slice the caller can
// scan for the ones it cares about (most code just needs the
// message type).
type Message struct {
	Op     uint8
	HType  uint8
	HLen   uint8
	Hops   uint8
	XID    uint32
	Secs   uint16
	Flags  uint16
	CIAddr [4]byte
	YIAddr [4]byte
	SIAddr [4]byte
	GIAddr [4]byte
	// CHAddr is 16 bytes on the wire; only the first HLen bytes
	// are the MAC. We keep the full 16 so round-tripping doesn't
	// lose the zero padding.
	CHAddr [16]byte
	SName  [64]byte
	File   [128]byte
	// Options in the order they appeared on the wire. Duplicates
	// are preserved even though most clients don't send them.
	Options []Option
}

// Option is a single DHCP option (code + length-prefixed data).
type Option struct {
	Code uint8
	Data []byte
}

// Parse decodes a DHCP packet. Returns a descriptive error for
// malformed input — we intentionally do not try to be maximally
// lenient because the "client" here is our own guest kernel's
// dhclient, which is well-behaved.
func Parse(buf []byte) (*Message, error) {
	if len(buf) < minPacketLen {
		return nil, fmt.Errorf("dhcp: packet too short (%d < %d)", len(buf), minPacketLen)
	}
	m := &Message{
		Op:    buf[0],
		HType: buf[1],
		HLen:  buf[2],
		Hops:  buf[3],
		XID:   binary.BigEndian.Uint32(buf[4:8]),
		Secs:  binary.BigEndian.Uint16(buf[8:10]),
		Flags: binary.BigEndian.Uint16(buf[10:12]),
	}
	copy(m.CIAddr[:], buf[12:16])
	copy(m.YIAddr[:], buf[16:20])
	copy(m.SIAddr[:], buf[20:24])
	copy(m.GIAddr[:], buf[24:28])
	copy(m.CHAddr[:], buf[28:44])
	copy(m.SName[:], buf[44:108])
	copy(m.File[:], buf[108:236])
	magic := binary.BigEndian.Uint32(buf[headerLen : headerLen+magicLen])
	if magic != magicCookie {
		return nil, fmt.Errorf("dhcp: bad magic 0x%08x (want 0x%08x)", magic, magicCookie)
	}
	opts, err := parseOptions(buf[headerLen+magicLen:])
	if err != nil {
		return nil, err
	}
	m.Options = opts
	return m, nil
}

// parseOptions walks the TLV-encoded option stream.
//
// Special codes:
//
//   0   (pad): skip one byte, no length. Used for alignment;
//              common as a no-op filler.
//   255 (end): stop parsing. Anything after is padding.
//
// All other codes are followed by a 1-byte length and then length
// data bytes.
func parseOptions(buf []byte) ([]Option, error) {
	var opts []Option
	i := 0
	for i < len(buf) {
		code := buf[i]
		if code == optPad {
			i++
			continue
		}
		if code == optEnd {
			return opts, nil
		}
		if i+1 >= len(buf) {
			return nil, fmt.Errorf("dhcp: truncated option header at byte %d", i)
		}
		length := int(buf[i+1])
		if i+2+length > len(buf) {
			return nil, fmt.Errorf("dhcp: option %d length %d exceeds remaining %d bytes",
				code, length, len(buf)-i-2)
		}
		data := make([]byte, length)
		copy(data, buf[i+2:i+2+length])
		opts = append(opts, Option{Code: code, Data: data})
		i += 2 + length
	}
	// No explicit optEnd but we ran out of buffer — accept this
	// for compatibility with clients that omit it.
	return opts, nil
}

// Serialize encodes a Message back to the wire format. Always
// terminates the options stream with optEnd so downstream
// consumers don't have to guess.
func Serialize(m *Message) []byte {
	buf := make([]byte, minPacketLen, minPacketLen+128)
	buf[0] = m.Op
	buf[1] = m.HType
	buf[2] = m.HLen
	buf[3] = m.Hops
	binary.BigEndian.PutUint32(buf[4:8], m.XID)
	binary.BigEndian.PutUint16(buf[8:10], m.Secs)
	binary.BigEndian.PutUint16(buf[10:12], m.Flags)
	copy(buf[12:16], m.CIAddr[:])
	copy(buf[16:20], m.YIAddr[:])
	copy(buf[20:24], m.SIAddr[:])
	copy(buf[24:28], m.GIAddr[:])
	copy(buf[28:44], m.CHAddr[:])
	copy(buf[44:108], m.SName[:])
	copy(buf[108:236], m.File[:])
	binary.BigEndian.PutUint32(buf[headerLen:headerLen+magicLen], magicCookie)

	for _, o := range m.Options {
		buf = append(buf, o.Code, byte(len(o.Data)))
		buf = append(buf, o.Data...)
	}
	buf = append(buf, optEnd)
	return buf
}

// MessageType extracts option 53. Returns (0, false) if absent.
// This is the single option every DHCP packet must have; absence
// means the peer is speaking a different protocol.
func (m *Message) MessageType() (uint8, bool) {
	for _, o := range m.Options {
		if o.Code == optMessageType && len(o.Data) >= 1 {
			return o.Data[0], true
		}
	}
	return 0, false
}

// RequestedIP extracts option 50. Clients include this in
// DHCPREQUEST to say "I'd like this specific IP" (typically the
// one they received in a prior OFFER, or one from a cached lease
// they'd like renewed).
func (m *Message) RequestedIP() (netip.Addr, bool) {
	for _, o := range m.Options {
		if o.Code == optRequestedIP && len(o.Data) == 4 {
			return netip.AddrFrom4([4]byte{o.Data[0], o.Data[1], o.Data[2], o.Data[3]}), true
		}
	}
	return netip.Addr{}, false
}

// ClientMAC returns the MAC address from CHAddr using the first
// HLen bytes. Returns an error if HLen isn't 6 (Ethernet).
func (m *Message) ClientMAC() ([6]byte, error) {
	if m.HLen != hlenMAC {
		return [6]byte{}, fmt.Errorf("dhcp: unexpected HLen=%d (want %d)", m.HLen, hlenMAC)
	}
	var mac [6]byte
	copy(mac[:], m.CHAddr[:6])
	return mac, nil
}

// --- helpers used by responder.go to build reply options ------

// OptMsgType builds option 53.
func OptMsgType(t uint8) Option { return Option{Code: optMessageType, Data: []byte{t}} }

// OptServerID builds option 54.
func OptServerID(ip netip.Addr) Option {
	b := ip.As4()
	return Option{Code: optServerID, Data: b[:]}
}

// OptSubnetMask builds option 1.
func OptSubnetMask(mask [4]byte) Option {
	d := make([]byte, 4)
	copy(d, mask[:])
	return Option{Code: optSubnetMask, Data: d}
}

// OptRouter builds option 3 (default gateway).
func OptRouter(ip netip.Addr) Option {
	b := ip.As4()
	return Option{Code: optRouter, Data: b[:]}
}

// OptDNSServer builds option 6.
func OptDNSServer(ip netip.Addr) Option {
	b := ip.As4()
	return Option{Code: optDNSServer, Data: b[:]}
}

// OptLeaseTime builds option 51 (seconds).
func OptLeaseTime(secs uint32) Option {
	d := make([]byte, 4)
	binary.BigEndian.PutUint32(d, secs)
	return Option{Code: optLeaseTime, Data: d}
}

// MaskFromPrefixBits turns a /N network length into the 4-byte
// IPv4 subnet mask. /30 → 255.255.255.252.
func MaskFromPrefixBits(bits int) ([4]byte, error) {
	if bits < 0 || bits > 32 {
		return [4]byte{}, errors.New("dhcp: prefix bits out of range")
	}
	var m uint32
	if bits > 0 {
		m = ^uint32(0) << (32 - bits)
	}
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], m)
	return out, nil
}
