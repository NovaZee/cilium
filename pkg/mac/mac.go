// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package mac

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
)

// Untagged ethernet (IEEE 802.3) frame header len
const EthHdrLen = 14

// Uint64MAC is the __u64 representation of a MAC address.
// It corresponds to the C mac_t type used in bpf/.
type Uint64MAC uint64

func (m Uint64MAC) String() string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X",
		uint64((m & 0x0000000000FF)),
		uint64((m&0x00000000FF00)>>8),
		uint64((m&0x000000FF0000)>>16),
		uint64((m&0x0000FF000000)>>24),
		uint64((m&0x00FF00000000)>>32),
		uint64((m&0xFF0000000000)>>40),
	)
}

// MAC address is an net.HardwareAddr encapsulation to force cilium to only use MAC-48.
type MAC net.HardwareAddr

// String returns the string representation of m.
func (m MAC) String() string {
	return net.HardwareAddr(m).String()
}

// As8 returns the MAC as an array of 8 bytes for use in datapath configuration
// structs. This is 8 bytes due to padding of union macaddr.
func (m MAC) As8() [8]byte {
	var res [8]byte
	copy(res[:], m)
	return res
}

// ParseMAC parses s only as an IEEE 802 MAC-48.
func ParseMAC(s string) (MAC, error) {
	ha, err := net.ParseMAC(s)
	if err != nil {
		return nil, err
	}
	if len(ha) != 6 {
		return nil, fmt.Errorf("invalid MAC address %s", s)
	}

	return MAC(ha), nil
}

// MustParseMAC calls [ParseMAC] and panics on error. It is intended for use in tests with
// hard-coded strings.
func MustParseMAC(s string) MAC {
	mac, err := ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return mac
}

// Uint64 returns the MAC in uint64 format. The MAC is represented as little-endian in
// the returned value.
// Example:
//
//	m := MAC([]{0x11, 0x12, 0x23, 0x34, 0x45, 0x56})
//	v, err := m.Uint64()
//	fmt.Printf("0x%X", v) // 0x564534231211
func (m MAC) Uint64() (Uint64MAC, error) {
	if len(m) != 6 {
		return 0, fmt.Errorf("invalid MAC address %s", m.String())
	}

	res := uint64(m[5])<<40 | uint64(m[4])<<32 | uint64(m[3])<<24 |
		uint64(m[2])<<16 | uint64(m[1])<<8 | uint64(m[0])
	return Uint64MAC(res), nil
}

func (m MAC) MarshalJSON() ([]byte, error) {
	if len(m) == 0 {
		return []byte(`""`), nil
	}
	if len(m) != 6 {
		return nil, fmt.Errorf("invalid MAC address length %s", string(m))
	}
	return fmt.Appendf(nil, "\"%02x:%02x:%02x:%02x:%02x:%02x\"", m[0], m[1], m[2], m[3], m[4], m[5]), nil
}

func (m MAC) MarshalIndentJSON(prefix, indent string) ([]byte, error) {
	return m.MarshalJSON()
}

func (m *MAC) UnmarshalJSON(data []byte) error {
	if len(data) == len([]byte(`""`)) {
		if m == nil {
			m = new(MAC)
		}
		*m = MAC{}
		return nil
	}
	if len(data) != 19 {
		return fmt.Errorf("invalid MAC address length %s", string(data))
	}
	data = data[1 : len(data)-1]
	macStr := bytes.ReplaceAll(data, []byte(`:`), []byte(``))
	if len(macStr) != 12 {
		return fmt.Errorf("invalid MAC address format")
	}
	macByte := make([]byte, len(macStr))
	hex.Decode(macByte, macStr)
	*m = MAC{macByte[0], macByte[1], macByte[2], macByte[3], macByte[4], macByte[5]}
	return nil
}

// GenerateRandMAC generates a random unicast and locally administered MAC address.
func GenerateRandMAC() (MAC, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("Unable to retrieve 6 rnd bytes: %w", err)
	}

	// Set locally administered addresses bit and reset multicast bit
	buf[0] = (buf[0] | 0x02) & 0xfe

	return buf, nil
}

// CArrayString returns a string which can be used for assigning the given
// MAC addr to "union macaddr" in C.
func CArrayString(m net.HardwareAddr) string {
	if m == nil || len(m) != 6 {
		return "{0x0,0x0,0x0,0x0,0x0,0x0}"
	}

	return fmt.Sprintf("{0x%x,0x%x,0x%x,0x%x,0x%x,0x%x}",
		m[0], m[1], m[2], m[3], m[4], m[5])
}

// GenerateMACAddressFromPodNamespaceAndName generates a unique MAC address based on namespace and name
func GenerateMACAddressFromPodNamespaceAndName(ns, podName string) (MAC, error) {
	// Combine namespace and name to ensure uniqueness
	input := ns + "/" + podName

	// Create a SHA-256 hasher
	hasher := sha256.New()
	// Write the combined input string to the hasher
	hasher.Write([]byte(input))
	// Compute the SHA-256 hash
	hash := hasher.Sum(nil)
	// Take the first 6 bytes of the hash
	mac := hash[:6]
	// Set locally administered addresses bit and reset multicast bit
	mac[0] = (mac[0] | 0x02) & 0xfe
	// Format the MAC address in the usual colon-separated hex notation
	return MAC(mac), nil
}

// MACConfig specifies the configuration for MAC address generation.
type MACConfig struct {
	// FixedMAC specifies a global fixed MAC address for all pods.
	// When set, all pods will use this MAC address regardless of other settings.
	FixedMAC string
	// PrefixMACMap specifies a mapping of pod name prefixes to fixed MAC addresses.
	// Pods whose names match a prefix will use the corresponding MAC address.
	PrefixMACMap map[string]string
	// MACAddrMode specifies the MAC address generation mode.
	// Valid values: "random" (default), "deterministic"
	MACAddrMode string
	// PodNamespace is the Kubernetes namespace of the pod, used for deterministic MAC generation.
	PodNamespace string
	// PodName is the Kubernetes pod name, used for deterministic MAC generation and prefix matching.
	PodName string
}

// GenerateMACWithConfig generates a MAC address based on the provided configuration.
// The priority order is:
// 1. FixedMAC (global fixed MAC)
// 2. PrefixMACMap (prefix-based fixed MAC)
// 3. Deterministic (based on namespace and pod name)
// 4. Random (default)
func GenerateMACWithConfig(cfg MACConfig) (MAC, error) {
	// 1. Check for global fixed MAC
	if cfg.FixedMAC != "" {
		return ParseMAC(cfg.FixedMAC)
	}

	// 2. Check for prefix-based fixed MAC
	if len(cfg.PrefixMACMap) > 0 && cfg.PodName != "" {
		for prefix, macAddr := range cfg.PrefixMACMap {
			if strings.HasPrefix(cfg.PodName, prefix) {
				return ParseMAC(macAddr)
			}
		}
	}
	// 固定pod mac地址 设计整理
	// 3. Check for deterministic mode
	if cfg.MACAddrMode == "deterministic" && cfg.PodNamespace != "" && cfg.PodName != "" {
		return GenerateMACAddressFromPodNamespaceAndName(cfg.PodNamespace, cfg.PodName)
	}

	// 4. Default to random generation
	return GenerateRandMAC()
}
