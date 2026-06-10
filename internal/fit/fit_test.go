package fit

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/matthieudolci/sync2connect/internal/model"
)

func TestChecksumKnownVector(t *testing.T) {
	// FIT uses CRC-16/ARC; the standard check value for "123456789" is 0xBB3D.
	if got := Checksum([]byte("123456789")); got != 0xBB3D {
		t.Fatalf("Checksum(123456789) = %#x, want 0xBB3D", got)
	}
}

// parsedMessage is one decoded data message: global number -> field values.
type parsedMessage struct {
	global uint16
	fields map[byte][]byte
}

// parseFit is a minimal little-endian FIT decoder used to validate encoder
// output structurally.
func parseFit(t *testing.T, data []byte) []parsedMessage {
	t.Helper()
	if len(data) < headerSize+2 {
		t.Fatalf("file too short: %d bytes", len(data))
	}
	if data[0] != headerSize {
		t.Fatalf("header size = %d, want %d", data[0], headerSize)
	}
	if string(data[8:12]) != ".FIT" {
		t.Fatalf("magic = %q, want .FIT", data[8:12])
	}
	dataSize := binary.LittleEndian.Uint32(data[4:8])
	if int(dataSize) != len(data)-headerSize-2 {
		t.Fatalf("declared data size %d, actual %d", dataSize, len(data)-headerSize-2)
	}
	wantCRC := binary.LittleEndian.Uint16(data[len(data)-2:])
	if got := Checksum(data[:len(data)-2]); got != wantCRC {
		t.Fatalf("file CRC = %#x, trailing CRC = %#x", got, wantCRC)
	}

	type definition struct {
		global uint16
		fields []fieldDef
	}
	defs := map[byte]definition{}
	var messages []parsedMessage

	records := data[headerSize : len(data)-2]
	for i := 0; i < len(records); {
		header := records[i]
		i++
		local := header & 0x0F
		if header&0x40 != 0 { // definition message
			arch := records[i+1]
			if arch != 0 {
				t.Fatalf("expected little-endian architecture, got %d", arch)
			}
			global := binary.LittleEndian.Uint16(records[i+2 : i+4])
			numFields := int(records[i+4])
			i += 5
			var fields []fieldDef
			for f := 0; f < numFields; f++ {
				fields = append(fields, fieldDef{records[i], records[i+1], records[i+2]})
				i += 3
			}
			defs[local] = definition{global: global, fields: fields}
			continue
		}
		def, ok := defs[local]
		if !ok {
			t.Fatalf("data message for undefined local type %d", local)
		}
		msg := parsedMessage{global: def.global, fields: map[byte][]byte{}}
		for _, f := range def.fields {
			msg.fields[f.num] = records[i : i+int(f.size)]
			i += int(f.size)
		}
		messages = append(messages, msg)
	}
	return messages
}

func TestEncodeWeight(t *testing.T) {
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	older := time.Date(2026, 5, 30, 8, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 31, 8, 0, 0, 0, time.UTC)

	// Pass measurements out of order to verify chronological sorting.
	data := EncodeWeight([]model.BodyMeasurement{
		{
			Timestamp:        newer,
			WeightKg:         81.25,
			BodyFatPercent:   model.Float(22.5),
			HydrationPercent: model.Float(55.1),
			BoneMassKg:       model.Float(3.2),
			MuscleMassKg:     model.Float(60.04),
			BMI:              model.Float(24.6),
		},
		{Timestamp: older, WeightKg: 82},
	}, created)

	messages := parseFit(t, data)
	if len(messages) != 3 {
		t.Fatalf("got %d messages, want 3 (file_id + 2 weight)", len(messages))
	}

	fileID := messages[0]
	if fileID.global != mesgFileID {
		t.Fatalf("first message global = %d, want file_id", fileID.global)
	}
	if got := fileID.fields[0][0]; got != fileTypeWeight {
		t.Fatalf("file_id type = %d, want %d (weight)", got, fileTypeWeight)
	}
	if got := binary.LittleEndian.Uint32(fileID.fields[4]); got != uint32(created.Unix()-fitEpochOffset) {
		t.Fatalf("file_id time_created = %d, want %d", got, created.Unix()-fitEpochOffset)
	}

	u16 := func(m parsedMessage, num byte) uint16 {
		t.Helper()
		raw, ok := m.fields[num]
		if !ok {
			t.Fatalf("field %d missing", num)
		}
		return binary.LittleEndian.Uint16(raw)
	}

	first, second := messages[1], messages[2]
	if first.global != mesgWeightScale || second.global != mesgWeightScale {
		t.Fatalf("weight messages have globals %d, %d", first.global, second.global)
	}
	// Chronological order: the "older" measurement comes first.
	if got := binary.LittleEndian.Uint32(first.fields[253]); got != uint32(older.Unix()-fitEpochOffset) {
		t.Fatalf("first weight timestamp = %d, want older measurement", got)
	}
	if got := u16(first, 0); got != 8200 {
		t.Fatalf("weight = %d, want 8200 (82.00 kg)", got)
	}
	// Optional fields absent on the first measurement must be invalid.
	for _, num := range []byte{1, 2, 4, 5, 13} {
		if got := u16(first, num); got != invalidUint16 {
			t.Fatalf("optional field %d = %d, want invalid", num, got)
		}
	}

	checks := map[byte]uint16{
		0:  8125, // 81.25 kg * 100
		1:  2250, // 22.5 % * 100
		2:  5510, // 55.1 % * 100
		4:  320,  // 3.2 kg * 100
		5:  6004, // 60.04 kg * 100
		13: 246,  // 24.6 BMI * 10
	}
	for num, want := range checks {
		if got := u16(second, num); got != want {
			t.Fatalf("field %d = %d, want %d", num, got, want)
		}
	}
}

func TestScaledOutOfRange(t *testing.T) {
	if got := scaled(model.Float(700), 100); got != invalidUint16 {
		t.Fatalf("scaled(700, 100) = %d, want invalid", got)
	}
	if got := scaled(model.Float(-1), 100); got != invalidUint16 {
		t.Fatalf("scaled(-1, 100) = %d, want invalid", got)
	}
	if got := scaled(nil, 100); got != invalidUint16 {
		t.Fatalf("scaled(nil) = %d, want invalid", got)
	}
}
