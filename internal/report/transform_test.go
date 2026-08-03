package report

import "testing"

// These three helpers shape strings on their way into metric attributes. They are
// covered directly because each replaced a hand-rolled implementation: a silent
// regression here corrupts attribute values rather than failing a poll.

func TestFormatMAC(t *testing.T) {
	tests := []struct {
		raw  []byte
		want string
	}{
		{[]byte{0xf4, 0x7f, 0x35, 0x93, 0xaf, 0x80}, "f4:7f:35:93:af:80"},
		// Leading zeros must be kept: a device reporting 00:0c:29:... must not
		// collapse to 0:c:29:...
		{[]byte{0x00, 0x0c, 0x29, 0x01, 0x02, 0x03}, "00:0c:29:01:02:03"},
		{[]byte{}, ""},
	}
	for _, tc := range tests {
		if got := formatMAC(tc.raw); got != tc.want {
			t.Errorf("formatMAC(% x) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestFormatIP(t *testing.T) {
	tests := []struct {
		name    string
		raw     []byte
		want    string
		wantErr bool
	}{
		{"ipv4", []byte{10, 0, 0, 1}, "10.0.0.1", false},
		{"ipv4 zero", []byte{0, 0, 0, 0}, "0.0.0.0", false},
		// IPv6 comes out compressed, which is the canonical form.
		{"ipv6", []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01}, "2001:db8::1", false},
		{"wrong length", []byte{1, 2, 3, 4, 5}, "", true},
		{"empty", nil, "", true},
	}
	for _, tc := range tests {
		got, err := formatIP(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error, got %q", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: formatIP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestExpandTemplate(t *testing.T) {
	tests := []struct {
		template string
		want     string
	}{
		{"$1", "${1}"},
		// The reason this function exists: without braces, Go reads the group name
		// as "1x" and expands to nothing.
		{"$1x", "${1}x"},
		{"$1-$2", "${1}-${2}"},
		{"$12", "${12}"},
		{"port $1 of $2", "port ${1} of ${2}"},
		// Already braced, a bare dollar, and an escaped dollar all pass through.
		{"${1}", "${1}"},
		{"$", "$"},
		{"$$", "$$"},
		{"cost $ and $x", "cost $ and $x"},
		{"no variables", "no variables"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := expandTemplate(tc.template); got != tc.want {
			t.Errorf("expandTemplate(%q) = %q, want %q", tc.template, got, tc.want)
		}
	}
}

// TestComponentIdentity pins how hw.id is derived, since consumers join hw.*
// metrics for one component on it. Table names go through naming.Snake; an
// unnamed table falls back to its OID with the arcs flattened.
func TestComponentPrefix(t *testing.T) {
	tests := []struct {
		name, tableName, tableOID, want string
	}{
		// ifTable and ifXTable describe the same interfaces, so both must
		// canonicalise to "if" or their metrics could not be joined.
		{"ifTable canonical", "ifTable", "1.3.6.1.2.1.2.2", "if"},
		{"ifXTable canonical", "ifXTable", "1.3.6.1.2.1.31.1.1", "if"},
		{"leading dot still canonical", "ifTable", ".1.3.6.1.2.1.2.2", "if"},
		{"snake cased, Table suffix dropped", "entPhySensorTable", "1.3.6.1.2.1.99.1.1", "ent_phy_sensor"},
		{"acronym run kept together", "cpuHCUsageTable", "1.2.3", "cpu_hc_usage"},
		{"unnamed table falls back to a flattened OID", "", "1.3.6.1.4.1.9.9.13.1.3", "1_3_6_1_4_1_9_9_13_1_3"},
		{"unnamed table, leading dot", "", ".1.2.3", "1_2_3"},
	}
	for _, tc := range tests {
		if got := componentPrefix(tc.tableName, tc.tableOID); got != tc.want {
			t.Errorf("%s: componentPrefix(%q, %q) = %q, want %q",
				tc.name, tc.tableName, tc.tableOID, got, tc.want)
		}
	}
}

func TestComponentID(t *testing.T) {
	tests := []struct {
		component, index, symbolName, want string
	}{
		{"if", "1", "ifHCInOctets", "if_1"},
		{"ent_phy_sensor", "1.7", "entPhySensorValue", "ent_phy_sensor_1.7"},
		// A scalar has no row, so it is identified by its own snake_cased name.
		{"", "", "ciscoMemoryPoolUsed", "cisco_memory_pool_used"},
		{"", "", "", ""},
	}
	for _, tc := range tests {
		if got := componentID(tc.component, tc.index, tc.symbolName); got != tc.want {
			t.Errorf("componentID(%q, %q, %q) = %q, want %q",
				tc.component, tc.index, tc.symbolName, got, tc.want)
		}
	}
}
