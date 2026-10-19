package config

import (
	"time"

	kconfig "github.com/DavidGodefroid/lockkit/config"
)

// Body modes: how the string leaves of a JSON message body are masked.
const (
	// BodyRedactUnknown redacts every string leaf no allow rule names.
	BodyRedactUnknown = "redact-unknown"
	// BodyDetect masks what mask rules, key names and detectors find.
	BodyDetect = "detect"
)

// Hard ceilings of the per-request message counts.
const (
	ReadMaxCeiling = 200
	PeekMaxCeiling = 20
)

// Bounds of the session limits: MinIdleTimeout <= idle_timeout <=
// max_session <= MaxMaxSession.
const (
	MinIdleTimeout = time.Minute
	MaxMaxSession  = 24 * time.Hour
)

// DefaultDetectors is the detector set of a profile that sets none.
// National id detectors are opt-in.
func DefaultDetectors() []string {
	return []string{"card", "email", "iban", "phone"}
}

// DefaultLimits returns the limits applied to unset limit fields.
func DefaultLimits() Limits {
	return Limits{
		RequestTimeout:   10 * time.Second,
		ReadMax:          20,
		PeekMax:          5,
		DeliveryHeadroom: 3,
		MaxBodyBytes:     16384,
		MaxOutputBytes:   65536,
		ListPageSize:     100,
	}
}

// Default session limits.
const (
	DefaultIdleTimeout = 20 * time.Minute
	DefaultMaxSession  = 4 * time.Hour
)

// DefaultBodyMode is redact-unknown on production, detect elsewhere.
func DefaultBodyMode(production bool) string {
	if production {
		return BodyRedactUnknown
	}
	return BodyDetect
}

// applyDefaults fills the unset fields of p. detectorsSet and headroomSet
// report keys present in the file: an explicit empty detector list and an
// explicit headroom of 0 are kept.
func applyDefaults(p *Profile, detectorsSet, headroomSet bool) {
	if p.Credentials == "" {
		p.Credentials = kconfig.CredentialsAsk
	}
	if p.TLS == "" {
		p.TLS = kconfig.TLSVerifyFull
	}
	if !detectorsSet {
		p.Detectors = DefaultDetectors()
	}
	if p.BodyMode == "" {
		p.BodyMode = DefaultBodyMode(p.Production)
	}
	d := DefaultLimits()
	l := &p.Limits
	if l.RequestTimeout == 0 {
		l.RequestTimeout = d.RequestTimeout
	}
	if l.ReadMax == 0 {
		l.ReadMax = d.ReadMax
	}
	if l.PeekMax == 0 {
		l.PeekMax = d.PeekMax
	}
	if !headroomSet {
		l.DeliveryHeadroom = d.DeliveryHeadroom
	}
	if l.MaxBodyBytes == 0 {
		l.MaxBodyBytes = d.MaxBodyBytes
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = d.MaxOutputBytes
	}
	if l.ListPageSize == 0 {
		l.ListPageSize = d.ListPageSize
	}
	if p.MaxSession == 0 {
		p.MaxSession = DefaultMaxSession
	}
	if p.IdleTimeout == 0 {
		// A defaulted idle timeout never exceeds an explicit max_session.
		p.IdleTimeout = min(DefaultIdleTimeout, p.MaxSession)
	}
	p.Detectors = kconfig.SortedSet(p.Detectors)
	p.VHosts = kconfig.SortedSet(p.VHosts)
}
