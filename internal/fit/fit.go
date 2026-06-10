// Package fit implements the small subset of the Garmin FIT binary format
// needed to upload weight-scale data to Garmin Connect: a file header,
// little-endian definition and data records, and the FIT CRC-16.
package fit

import (
	"bytes"
	"encoding/binary"
)

// FIT base types used by this encoder.
const (
	baseEnum    = 0x00
	baseUint16  = 0x84
	baseUint32  = 0x86
	baseUint32z = 0x8C
)

// Invalid placeholder values per FIT base type, written when an optional
// field has no data.
const (
	invalidUint16 = 0xFFFF
)

// fitEpochOffset converts Unix time to the FIT epoch (1989-12-31T00:00:00Z).
const fitEpochOffset = 631065600

const (
	headerSize      = 12
	protocolVersion = 0x10 // 1.0
	profileVersion  = 2132 // 21.32
)

type fieldDef struct {
	num      byte
	size     byte
	baseType byte
}

// encoder accumulates FIT records and assembles the final file.
type encoder struct {
	records bytes.Buffer
}

// writeDefinition emits a definition message binding the local message type
// to a global message number with the given fields (little-endian).
func (e *encoder) writeDefinition(local byte, global uint16, fields []fieldDef) {
	e.records.WriteByte(0x40 | local) // definition record header
	e.records.WriteByte(0)            // reserved
	e.records.WriteByte(0)            // architecture: little-endian
	var num [2]byte
	binary.LittleEndian.PutUint16(num[:], global)
	e.records.Write(num[:])
	e.records.WriteByte(byte(len(fields)))
	for _, f := range fields {
		e.records.Write([]byte{f.num, f.size, f.baseType})
	}
}

// writeData emits a data message for the local message type. The payload
// must match the sizes declared in the corresponding definition.
func (e *encoder) writeData(local byte, payload []byte) {
	e.records.WriteByte(local)
	e.records.Write(payload)
}

// file assembles the header, records and trailing CRC into a complete file.
func (e *encoder) file() []byte {
	header := make([]byte, headerSize)
	header[0] = headerSize
	header[1] = protocolVersion
	binary.LittleEndian.PutUint16(header[2:], profileVersion)
	binary.LittleEndian.PutUint32(header[4:], uint32(e.records.Len()))
	copy(header[8:], ".FIT")

	out := make([]byte, 0, headerSize+e.records.Len()+2)
	out = append(out, header...)
	out = append(out, e.records.Bytes()...)
	crc := Checksum(out)
	return append(out, byte(crc), byte(crc>>8))
}

var crcTable = [16]uint16{
	0x0000, 0xCC01, 0xD801, 0x1400, 0xF001, 0x3C00, 0x2800, 0xE401,
	0xA001, 0x6C00, 0x7800, 0xB401, 0x5000, 0x9C01, 0x8801, 0x4400,
}

// Checksum computes the FIT CRC-16 (equivalent to CRC-16/ARC) over data.
func Checksum(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		tmp := crcTable[crc&0xF]
		crc = (crc >> 4) & 0x0FFF
		crc = crc ^ tmp ^ crcTable[b&0xF]

		tmp = crcTable[crc&0xF]
		crc = (crc >> 4) & 0x0FFF
		crc = crc ^ tmp ^ crcTable[(b>>4)&0xF]
	}
	return crc
}
