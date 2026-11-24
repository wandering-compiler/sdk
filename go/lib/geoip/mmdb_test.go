package geoip

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"sort"
)

// buildMMDB writes a minimal MaxMind DB (IPv6 tree, 24-bit records) in which
// each prefix maps to {"country": {"iso_code": code}} — enough for the
// reader's Verify and a country lookup, with no writer dependency.
func buildMMDB(entries map[string]string) []byte {
	type node struct{ rec [2]int } // -1 empty, >=0 node, <-1 data: -(2+i)
	nodes := []node{{rec: [2]int{-1, -1}}}

	// The data section: one map per distinct country.
	var data bytes.Buffer
	offsets := map[string]int{}
	codes := make([]string, 0)
	for _, c := range entries {
		if _, ok := offsets[c]; !ok {
			offsets[c] = -1
			codes = append(codes, c)
		}
	}
	sort.Strings(codes)
	for _, c := range codes {
		offsets[c] = data.Len()
		writeMap(&data, 1)
		writeString(&data, "country")
		writeMap(&data, 1)
		writeString(&data, "iso_code")
		writeString(&data, c)
	}

	prefixes := make([]string, 0, len(entries))
	for p := range entries {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, ps := range prefixes {
		p := netip.MustParsePrefix(ps)
		a16 := p.Addr().As16()
		bits := p.Bits()
		if p.Addr().Is4() {
			bits += 96
			a16 = netip.AddrFrom16(netip.AddrFrom4(p.Addr().As4()).As16()).As16()
			// IPv4 lives at ::a.b.c.d (96 zero bits), not ::ffff:a.b.c.d.
			for i := 0; i < 12; i++ {
				a16[i] = 0
			}
		}
		n := 0
		for i := 0; i < bits; i++ {
			bit := int(a16[i/8]>>(7-uint(i%8))) & 1
			if i == bits-1 {
				nodes[n].rec[bit] = -(2 + offsets[entries[ps]])
				break
			}
			next := nodes[n].rec[bit]
			if next < 0 {
				nodes = append(nodes, node{rec: [2]int{-1, -1}})
				next = len(nodes) - 1
				nodes[n].rec[bit] = next
			}
			n = next
		}
	}

	count := len(nodes)
	var out bytes.Buffer
	put24 := func(v int) { out.Write([]byte{byte(v >> 16), byte(v >> 8), byte(v)}) }
	for _, nd := range nodes {
		for _, r := range nd.rec {
			switch {
			case r == -1:
				put24(count)
			case r >= 0:
				put24(r)
			default:
				put24(count + 16 + (-r - 2))
			}
		}
	}
	out.Write(make([]byte, 16))
	out.Write(data.Bytes())
	out.WriteString("\xAB\xCD\xEFMaxMind.com")
	writeMap(&out, 9)
	writeString(&out, "binary_format_major_version")
	writeUint16(&out, 2)
	writeString(&out, "binary_format_minor_version")
	writeUint16(&out, 0)
	writeString(&out, "build_epoch")
	out.Write([]byte{0<<5 | 8, 9 - 7}) // uint64, 8 bytes
	_ = binary.Write(&out, binary.BigEndian, uint64(1763856000))
	writeString(&out, "database_type")
	writeString(&out, "Test-Country")
	writeString(&out, "description")
	writeMap(&out, 1)
	writeString(&out, "en")
	writeString(&out, "test")
	writeString(&out, "ip_version")
	writeUint16(&out, 6)
	writeString(&out, "languages")
	out.Write([]byte{0<<5 | 1, 11 - 7}) // array of 1
	writeString(&out, "en")
	writeString(&out, "node_count")
	out.Write([]byte{6<<5 | 4})                             // uint32
	_ = binary.Write(&out, binary.BigEndian, uint32(count)) // #nosec G115 -- a test tree
	writeString(&out, "record_size")
	writeUint16(&out, 24)
	return out.Bytes()
}

func writeMap(b *bytes.Buffer, n int)    { b.WriteByte(byte(7<<5 | n)) }
func writeUint16(b *bytes.Buffer, v int) { b.Write([]byte{5<<5 | 2, byte(v >> 8), byte(v)}) }
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte(byte(2<<5 | len(s)))
	b.WriteString(s)
}
