// Package model defines the provider-agnostic data types that flow between
// source and destination providers.
package model

import "time"

// BodyMeasurement is a single body-composition reading. WeightKg is always
// set; the remaining fields are optional and nil when the source did not
// report them.
type BodyMeasurement struct {
	Timestamp        time.Time
	WeightKg         float64
	BodyFatPercent   *float64
	HydrationPercent *float64
	BoneMassKg       *float64
	MuscleMassKg     *float64
	BMI              *float64
}

// Float returns a pointer to v, for filling optional measurement fields.
func Float(v float64) *float64 { return &v }
