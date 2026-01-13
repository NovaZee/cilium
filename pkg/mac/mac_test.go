// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package mac

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	macTests := []struct {
		in      string
		out     MAC
		wantErr string
	}{
		{"00:00:5e:00:53:01", MAC{0x00, 0x00, 0x5e, 0x00, 0x53, 0x01}, ""},
		{"00-00-5e-00-53-01", MAC{0x00, 0x00, 0x5e, 0x00, 0x53, 0x01}, ""},
		{"0000.5e00.5301", MAC{0x00, 0x00, 0x5e, 0x00, 0x53, 0x01}, ""},

		// invalid delimiter
		{
			"01.02.03.04.05.06",
			nil,
			"invalid MAC address",
		},
		// not IEEE 802 MAC-48
		{
			"00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01",
			nil,
			"invalid MAC address",
		},
		{
			"00-00-00-00-fe-80-00-00-00-00-00-00-02-00-5e-10-00-00-00-01",
			nil,
			"invalid MAC address",
		},
		{
			"0000.0000.fe80.0000.0000.0000.0200.5e10.0000.0001",
			nil,
			"invalid MAC address",
		},
	}

	for _, tt := range macTests {
		t.Run(tt.in, func(t *testing.T) {
			out, err := ParseMAC(tt.in)
			require.Equal(t, tt.out, out)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
				require.Panics(t, func() { _ = MustParseMAC(tt.in) })
			}
		})
	}
}

func TestUint64(t *testing.T) {
	m := MAC([]byte{0x11, 0x12, 0x23, 0x34, 0x45, 0x56})
	v, err := m.Uint64()
	require.NoError(t, err)
	require.Equal(t, Uint64MAC(0x564534231211), v)
}

func TestUnmarshalJSON(t *testing.T) {
	m := MAC([]byte{0x11, 0x12, 0x23, 0x34, 0x45, 0x56})
	w := MAC([]byte{0x11, 0x12, 0x23, 0x34, 0x45, 0xAB})
	d, err := json.Marshal(m)
	require.NoError(t, err)
	require.Equal(t, []byte(`"11:12:23:34:45:56"`), d)
	var t1 MAC
	err = json.Unmarshal([]byte(`"11:12:23:34:45:AB"`), &t1)
	require.NoError(t, err)
	require.Equal(t, w, t1)
	err = json.Unmarshal([]byte(`"11:12:23:34:45:A"`), &t1)
	require.Error(t, err)

	m = MAC([]byte{})
	w = MAC([]byte{})
	d, err = json.Marshal(m)
	require.NoError(t, err)
	require.Equal(t, []byte(`""`), d)
	var t2 MAC
	err = json.Unmarshal([]byte(`""`), &t2)
	require.NoError(t, err)
	require.Equal(t, w, t2)
}

func TestGenerateMACWithConfig(t *testing.T) {
	tests := []struct {
		name         string
		cfg          MACConfig
		wantErr      bool
		validateMAC  bool
		checkMAC     func(MAC) bool
	}{
		{
			name: "Fixed MAC - highest priority",
			cfg: MACConfig{
				FixedMAC:     "AA:BB:CC:00:00:01",
				PrefixMACMap: map[string]string{"sts-": "AA:BB:CC:00:00:02"},
				MACAddrMode:  "deterministic",
				PodNamespace: "default",
				PodName:      "sts-0",
			},
			wantErr:     false,
			validateMAC: true,
			checkMAC: func(m MAC) bool {
				return strings.EqualFold(m.String(), "AA:BB:CC:00:00:01")
			},
		},
		{
			name: "Prefix MAC - pod matches prefix",
			cfg: MACConfig{
				PrefixMACMap: map[string]string{
					"sts-": "AA:BB:CC:00:00:02",
					"app-": "AA:BB:CC:00:00:03",
				},
				MACAddrMode:  "deterministic",
				PodNamespace: "default",
				PodName:      "sts-0",
			},
			wantErr:     false,
			validateMAC: true,
			checkMAC: func(m MAC) bool {
				return strings.EqualFold(m.String(), "AA:BB:CC:00:00:02")
			},
		},
		{
			name: "Prefix MAC - pod does not match any prefix",
			cfg: MACConfig{
				PrefixMACMap: map[string]string{
					"sts-": "AA:BB:CC:00:00:02",
					"app-": "AA:BB:CC:00:00:03",
				},
				MACAddrMode:  "deterministic",
				PodNamespace: "default",
				PodName:      "web-0",
			},
			wantErr:     false,
			validateMAC: true,
			checkMAC: func(m MAC) bool {
				// Should fall through to deterministic mode
				return m.String() != "AA:BB:CC:00:00:02" && m.String() != "AA:BB:CC:00:00:03"
			},
		},
		{
			name: "Deterministic mode",
			cfg: MACConfig{
				MACAddrMode:  "deterministic",
				PodNamespace: "default",
				PodName:      "test-pod",
			},
			wantErr:     false,
			validateMAC: true,
			checkMAC: func(m MAC) bool {
				// Same input should produce same MAC
				m2, _ := GenerateMACWithConfig(MACConfig{
					MACAddrMode:  "deterministic",
					PodNamespace: "default",
					PodName:      "test-pod",
				})
				return m.String() == m2.String()
			},
		},
		{
			name: "Deterministic mode - different pods produce different MACs",
			cfg: MACConfig{
				MACAddrMode:  "deterministic",
				PodNamespace: "default",
				PodName:      "test-pod-1",
			},
			wantErr:     false,
			validateMAC: true,
			checkMAC: func(m MAC) bool {
				m2, _ := GenerateMACWithConfig(MACConfig{
					MACAddrMode:  "deterministic",
					PodNamespace: "default",
					PodName:      "test-pod-2",
				})
				return m.String() != m2.String()
			},
		},
		{
			name: "Random mode - default",
			cfg: MACConfig{
				PodNamespace: "default",
				PodName:      "test-pod",
			},
			wantErr:     false,
			validateMAC: false, // Random MAC, no specific value to check
			checkMAC:     nil,
		},
		{
			name: "Random mode - explicitly set",
			cfg: MACConfig{
				MACAddrMode:  "random",
				PodNamespace: "default",
				PodName:      "test-pod",
			},
			wantErr:     false,
			validateMAC: false, // Random MAC, no specific value to check
			checkMAC:     nil,
		},
		{
			name: "Fixed MAC - invalid format",
			cfg: MACConfig{
				FixedMAC: "invalid-mac",
			},
			wantErr:     true,
			validateMAC: false,
			checkMAC:     nil,
		},
		{
			name: "Prefix MAC - invalid format",
			cfg: MACConfig{
				PrefixMACMap: map[string]string{"sts-": "invalid"},
				PodName:      "sts-0",
			},
			wantErr:     true,
			validateMAC: false,
			checkMAC:     nil,
		},
		{
			name: "Deterministic mode - missing namespace",
			cfg: MACConfig{
				MACAddrMode: "deterministic",
				PodName:     "test-pod",
			},
			wantErr:     false, // Falls back to random
			validateMAC: false,
			checkMAC:     nil,
		},
		{
			name: "Deterministic mode - missing pod name",
			cfg: MACConfig{
				MACAddrMode:  "deterministic",
				PodNamespace: "default",
			},
			wantErr:     false, // Falls back to random
			validateMAC: false,
			checkMAC:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := GenerateMACWithConfig(tt.cfg)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if tt.validateMAC && err == nil {
				require.True(t, tt.checkMAC(m), "MAC validation failed: %s", m.String())
			}
		})
	}
}

func TestGenerateMACWithConfigPriority(t *testing.T) {
	// Test priority: FixedMAC > PrefixMACMap > Deterministic > Random

	t.Run("FixedMAC has highest priority", func(t *testing.T) {
		cfg := MACConfig{
			FixedMAC:     "AA:BB:CC:00:00:01",
			PrefixMACMap: map[string]string{"any-": "AA:BB:CC:00:00:02"},
			MACAddrMode:  "deterministic",
			PodNamespace: "ns1",
			PodName:      "any-pod",
		}
		m, err := GenerateMACWithConfig(cfg)
		require.NoError(t, err)
		require.True(t, strings.EqualFold(m.String(), "AA:BB:CC:00:00:01"), "MAC mismatch: %s", m.String())
	})

	t.Run("PrefixMACMap overrides deterministic", func(t *testing.T) {
		cfg := MACConfig{
			PrefixMACMap: map[string]string{"pre-": "AA:BB:CC:00:00:02"},
			MACAddrMode:  "deterministic",
			PodNamespace: "ns1",
			PodName:      "pre-test",
		}
		m, err := GenerateMACWithConfig(cfg)
		require.NoError(t, err)
		require.True(t, strings.EqualFold(m.String(), "AA:BB:CC:00:00:02"), "MAC mismatch: %s", m.String())
	})

	t.Run("Deterministic overrides random", func(t *testing.T) {
		cfg := MACConfig{
			MACAddrMode:  "deterministic",
			PodNamespace: "ns1",
			PodName:      "test",
		}
		m1, err := GenerateMACWithConfig(cfg)
		require.NoError(t, err)
		m2, err := GenerateMACWithConfig(cfg)
		require.NoError(t, err)
		require.Equal(t, m1.String(), m2.String())
	})
}

func TestGenerateMACAddressFromPodNamespaceAndName(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		podName     string
		consistent  bool // Should produce same MAC for same input
	}{
		{
			name:       "Same input produces same MAC",
			namespace:  "default",
			podName:    "test-pod",
			consistent: true,
		},
		{
			name:       "Different namespace produces different MAC",
			namespace:  "kube-system",
			podName:    "test-pod",
			consistent: false,
		},
		{
			name:       "Different pod name produces different MAC",
			namespace:  "default",
			podName:    "other-pod",
			consistent: false,
		},
	}

	// First run to establish baseline
	baseMAC, err := GenerateMACAddressFromPodNamespaceAndName("default", "test-pod")
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := GenerateMACAddressFromPodNamespaceAndName(tt.namespace, tt.podName)
			require.NoError(t, err)

			if tt.consistent {
				require.Equal(t, baseMAC.String(), m.String())
			} else {
				require.NotEqual(t, baseMAC.String(), m.String())
			}
		})
	}
}
