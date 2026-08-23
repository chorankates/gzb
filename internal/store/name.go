package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ErrNoDevice reports that nothing in the registry matched. Callers use it to
// tell "you named a device I do not know" apart from "you named it ambiguously",
// which need different advice.
var ErrNoDevice = errors.New("no such device")

// noDevice is a not-found error that says which device was missing, and how it
// was asked for, while still answering errors.Is(err, ErrNoDevice). Wrapping the
// sentinel with %w instead would append "no such device" to a sentence that
// already says so.
type noDevice struct{ msg string }

func (e *noDevice) Error() string { return e.msg }
func (e *noDevice) Unwrap() error { return ErrNoDevice }

func noSuchDevice(format string, args ...any) error {
	return &noDevice{msg: fmt.Sprintf(format, args...)}
}

// MaxNameLen bounds a name so one device cannot wreck the alignment of every
// listing that includes it.
const MaxNameLen = 64

// NormalizeName collapses the whitespace a shell leaves behind, so that
// `gzb name 0x90CB living   room  thermo` and a quoted argument agree.
func NormalizeName(s string) string { return strings.Join(strings.Fields(s), " ") }

// ValidateName reports whether a name can be stored and looked up again.
//
// The only real constraint is that a name must not look like an address:
// Resolve tries addresses first, so a device called "0x90CB" could never be
// reached by its own name, and worse, would answer to another device's address.
func ValidateName(name string) error {
	switch {
	case name == "":
		return errors.New("a name cannot be empty")
	case len(name) > MaxNameLen:
		return fmt.Errorf("a name cannot be longer than %d characters", MaxNameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("a name cannot contain control characters")
		}
	}
	if _, ok := ParseNodeID(name); ok {
		return fmt.Errorf("%q is a network address, not a name", name)
	}
	if looksLikeIEEE(name) {
		return fmt.Errorf("%q is an IEEE address, not a name", name)
	}
	return nil
}

// ParseNodeID reads a 16-bit network address in the 0xNNNN form that Zigbee
// tools print. The prefix is required: without it, a name like "1234" would be
// read as an address.
func ParseNodeID(s string) (uint16, bool) {
	if len(s) < 3 || !strings.EqualFold(s[:2], "0x") {
		return 0, false
	}
	v, err := strconv.ParseUint(s[2:], 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// looksLikeIEEE matches the colon-separated form the registry is keyed by.
func looksLikeIEEE(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 8 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.ParseUint(p, 16, 8); err != nil {
			return false
		}
	}
	return true
}

// SetName gives a device a human-friendly name.
//
// Names must be unique, because they are used to address devices: two sensors
// both called "thermo" would make `gzb interview thermo` unanswerable.
func (s *Store) SetName(ieee, name string) (*Device, error) {
	d, ok := s.Devices[strings.ToUpper(ieee)]
	if !ok {
		return nil, noSuchDevice("no device %s in the registry", ieee)
	}
	name = NormalizeName(name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if other, ok := s.byName(name); ok && other.IEEE != d.IEEE {
		return nil, fmt.Errorf("%q is already the name of %s", name, other.IEEE)
	}
	d.Name = name
	return d, nil
}

// ClearName removes a device's name, returning what it was.
func (s *Store) ClearName(ieee string) (*Device, string, error) {
	d, ok := s.Devices[strings.ToUpper(ieee)]
	if !ok {
		return nil, "", noSuchDevice("no device %s in the registry", ieee)
	}
	was := d.Name
	d.Name = ""
	return d, was, nil
}

// byName finds a device by its exact name, ignoring case.
func (s *Store) byName(name string) (*Device, bool) {
	for _, d := range s.Devices {
		if d.Name != "" && strings.EqualFold(d.Name, name) {
			return d, true
		}
	}
	return nil, false
}

// Resolve finds the device a user meant, from an IEEE address, a network
// address in hex, or a name.
//
// Names are matched loosely — ignoring case, then by prefix, then by any
// substring — so a device can be addressed as "thermo" rather than "living room
// thermo". Loose matching is only safe because it refuses to guess: a query
// matching several devices is an error naming them, not an arbitrary pick.
//
// Addresses are tried before names and matched exactly, so nothing a user can
// call a device changes what an address means.
func (s *Store) Resolve(query string) (*Device, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, noSuchDevice("no device given")
	}

	if d, ok := s.Devices[strings.ToUpper(q)]; ok {
		return d, nil
	}
	if id, ok := ParseNodeID(q); ok {
		if d, ok := s.ByNodeID(id); ok {
			return d, nil
		}
		return nil, noSuchDevice("no device at network address 0x%04X in the registry", id)
	}

	// List orders by last seen, which makes the candidate list in an ambiguity
	// error start with the device most likely to have been meant.
	fold := strings.ToLower(NormalizeName(q))
	var prefix, substring []*Device
	for _, d := range s.List() {
		if d.Name == "" {
			continue
		}
		switch name := strings.ToLower(d.Name); {
		case name == fold:
			return d, nil
		case strings.HasPrefix(name, fold):
			prefix = append(prefix, d)
		case strings.Contains(name, fold):
			substring = append(substring, d)
		}
	}
	// A prefix match beats a substring match outright: "bedroom" should find
	// "bedroom lamp" even though "second bedroom lamp" also contains it.
	for _, candidates := range [][]*Device{prefix, substring} {
		switch len(candidates) {
		case 0:
			continue
		case 1:
			return candidates[0], nil
		default:
			return nil, ambiguous(q, candidates)
		}
	}
	return nil, noSuchDevice("no device matching %q in the registry", q)
}

func ambiguous(query string, candidates []*Device) error {
	names := make([]string, 0, len(candidates))
	for _, d := range candidates {
		names = append(names, strconv.Quote(d.Name))
	}
	return fmt.Errorf("%q matches %d devices (%s); use the full name or an address",
		query, len(candidates), strings.Join(names, ", "))
}
