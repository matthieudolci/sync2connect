package fit

import (
	"encoding/binary"
	"math"
	"sort"
	"time"

	"github.com/matthieudolci/sync2connect/internal/model"
)

// Global FIT message numbers.
const (
	mesgFileID      = 0
	mesgWeightScale = 30
)

// file_id "type" value for weight files.
const fileTypeWeight = 9

// manufacturer 255 is the FIT "development" id, accepted by Garmin Connect
// for third-party weight uploads.
const manufacturerDevelopment = 255

// EncodeWeight builds a complete FIT weight file from the measurements,
// suitable for upload to Garmin Connect. Measurements are written in
// chronological order; created stamps the file_id message.
func EncodeWeight(measurements []model.BodyMeasurement, created time.Time) []byte {
	sorted := make([]model.BodyMeasurement, len(measurements))
	copy(sorted, measurements)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	e := &encoder{}

	// file_id message (local type 0).
	e.writeDefinition(0, mesgFileID, []fieldDef{
		{num: 3, size: 4, baseType: baseUint32z}, // serial_number
		{num: 4, size: 4, baseType: baseUint32},  // time_created
		{num: 1, size: 2, baseType: baseUint16},  // manufacturer
		{num: 2, size: 2, baseType: baseUint16},  // product
		{num: 0, size: 1, baseType: baseEnum},    // type
	})
	payload := make([]byte, 0, 13)
	payload = appendUint32(payload, uint32(created.Unix())) // nonzero serial
	payload = appendUint32(payload, fitTime(created))
	payload = appendUint16(payload, manufacturerDevelopment)
	payload = appendUint16(payload, 0)
	payload = append(payload, fileTypeWeight)
	e.writeData(0, payload)

	// weight_scale messages (local type 1).
	e.writeDefinition(1, mesgWeightScale, []fieldDef{
		{num: 253, size: 4, baseType: baseUint32}, // timestamp
		{num: 0, size: 2, baseType: baseUint16},   // weight, kg * 100
		{num: 1, size: 2, baseType: baseUint16},   // percent_fat * 100
		{num: 2, size: 2, baseType: baseUint16},   // percent_hydration * 100
		{num: 4, size: 2, baseType: baseUint16},   // bone_mass, kg * 100
		{num: 5, size: 2, baseType: baseUint16},   // muscle_mass, kg * 100
		{num: 13, size: 2, baseType: baseUint16},  // bmi * 10
	})
	for _, m := range sorted {
		row := make([]byte, 0, 16)
		row = appendUint32(row, fitTime(m.Timestamp))
		row = appendUint16(row, scaled(&m.WeightKg, 100))
		row = appendUint16(row, scaled(m.BodyFatPercent, 100))
		row = appendUint16(row, scaled(m.HydrationPercent, 100))
		row = appendUint16(row, scaled(m.BoneMassKg, 100))
		row = appendUint16(row, scaled(m.MuscleMassKg, 100))
		row = appendUint16(row, scaled(m.BMI, 10))
		e.writeData(1, row)
	}

	return e.file()
}

// fitTime converts a time to seconds since the FIT epoch.
func fitTime(t time.Time) uint32 {
	return uint32(t.Unix() - fitEpochOffset)
}

// scaled converts an optional float to a scaled uint16 FIT value, returning
// the invalid placeholder when the value is absent or out of range.
func scaled(v *float64, scale float64) uint16 {
	if v == nil {
		return invalidUint16
	}
	s := math.Round(*v * scale)
	if s < 0 || s >= invalidUint16 {
		return invalidUint16
	}
	return uint16(s)
}

func appendUint16(b []byte, v uint16) []byte {
	return binary.LittleEndian.AppendUint16(b, v)
}

func appendUint32(b []byte, v uint32) []byte {
	return binary.LittleEndian.AppendUint32(b, v)
}
