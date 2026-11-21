/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package typeurl

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Errors returned by the versioned registry. They are wrapped with context, so
// callers should test them with errors.Is.
var (
	// ErrConflict is returned when a type URL/version pair is already
	// registered, or when the same Go type would be registered under a
	// different type URL.
	ErrConflict = errors.New("registration conflict")

	// ErrFrozen is returned by mutating operations on a frozen Registry.
	ErrFrozen = errors.New("registry is frozen")

	// ErrVersionMismatch is returned when a message declares a version that is
	// not registered, when a caller selects a version that conflicts with the
	// one declared by the message, or when the destination type belongs to a
	// different version of the same type URL.
	ErrVersionMismatch = errors.New("version mismatch")

	// ErrTypeMismatch is returned when the Go type supplied by the caller is
	// not the type resolved for the message.
	ErrTypeMismatch = errors.New("type mismatch")
)

// VersionSeparator separates a base type URL from an encoded protocol version
// on the wire. A versioned entry for base "example.com/foo" and version "v2"
// is encoded as "example.com/foo#v2". Versions are path-escaped, so any string
// (including one containing the separator) round-trips.
const VersionSeparator = "#"

// DefaultVersion is the version of the historical, unversioned registration.
// It sorts before every other version and is encoded on the wire without a
// version suffix, keeping existing type URL strings byte-for-byte compatible.
const DefaultVersion = ""

// SelectFunc chooses among the versions registered for one base type URL. It
// is called with the sorted, non-empty set of available versions whenever the
// caller did not request a version and the message did not declare one. The
// return value must be present in versions.
//
// Selection must not depend on registration order. That is what makes decode
// results deterministic after a hot reload.
type SelectFunc func(versions []string) string

// DefaultSelector is the default selection rule:
//
//  1. Prefer DefaultVersion, preserving the historical unversioned
//     registration whenever it exists.
//  2. Otherwise choose the highest version by natural order (CompareVersions).
//
// Declared versions are never routed through the selector: a message that
// declares a version, or a caller that requests one, is resolved exactly and
// a missing version is reported as ErrVersionMismatch.
func DefaultSelector(versions []string) string {
	if len(versions) == 0 {
		return DefaultVersion
	}
	for _, v := range versions {
		if v == DefaultVersion {
			return v
		}
	}
	return versions[len(versions)-1]
}

// Version is the protocol version of a registered type. The zero value is the
// default, unversioned registration.
type Version string

// IsDefault reports whether v is the default, unversioned registration.
func (v Version) IsDefault() bool { return v == DefaultVersion }

// String returns the version as a plain string.
func (v Version) String() string { return string(v) }

// Source identifies where a registration came from. It makes conflicts
// attributable and lets a plugin hot-reload path unregister exactly the
// entries it contributed. An empty Source means "unspecified".
type Source struct {
	// Name identifies the origin, typically a plugin or package name.
	Name string
	// Detail optionally carries extra context such as the plugin path or a
	// build identifier.
	Detail string
}

func (s Source) String() string {
	switch {
	case s.Name == "":
		return s.Detail
	case s.Detail == "":
		return s.Name
	default:
		return s.Name + " (" + s.Detail + ")"
	}
}

// Entry describes one registered type at one protocol version.
type Entry struct {
	// TypeURL is the base type URL, without a version suffix.
	TypeURL string
	// Version is the protocol version of this registration.
	Version Version
	// GoType is the non-pointer Go type values decode to.
	GoType reflect.Type
	// IsProto reports whether values are encoded with protocol buffers. When
	// false, values are encoded as JSON. Registrations made through the
	// compatibility RegisterLegacy method always use JSON, even when the Go
	// type implements proto.Message.
	IsProto bool
	// Source records where the registration came from.
	Source Source
}

// WireTypeURL returns the type URL as it appears in an Any.
func (e Entry) WireTypeURL() string {
	return EncodeTypeURL(e.TypeURL, e.Version)
}

func (e Entry) String() string {
	return fmt.Sprintf("%s (version %q, %s, proto=%t, source %s)",
		e.TypeURL, e.Version, e.GoType, e.IsProto, e.Source)
}

// EncodeTypeURL combines a base type URL and version into the wire
// representation used in a protobuf Any. The default version returns base
// unchanged, preserving compatibility with existing encoders.
func EncodeTypeURL(base string, version Version) string {
	if version == DefaultVersion {
		return base
	}
	return base + VersionSeparator + url.PathEscape(string(version))
}

// DecodeTypeURL splits a wire type URL into its base URL and declared version.
// A URL without a separator decodes to the default version.
func DecodeTypeURL(raw string) (base string, version Version) {
	i := strings.LastIndex(raw, VersionSeparator)
	if i < 0 {
		return raw, DefaultVersion
	}
	decoded, err := url.PathUnescape(raw[i+len(VersionSeparator):])
	if err != nil {
		// Keep the malformed string as the base URL so the lookup fails
		// with an error naming exactly what was requested.
		return raw, DefaultVersion
	}
	return raw[:i], Version(decoded)
}

// CompareVersions orders versions naturally: runs of decimal digits compare
// numerically and all other bytes compare bytewise. The comparison is a strict
// total order: strings that are naturally equal but differ in spelling (for
// example "v1" and "v01" because of leading zeros) are ordered bytewise, so
// distinct registration keys never compare as 0 and the order is transitive for
// any set of versions. The default version sorts before every non-default
// version.
//
// It returns a negative number when a < b, zero when the strings are equal and
// a positive number when a > b.
func CompareVersions(a, b string) int {
	if a == b {
		return 0
	}
	if a == DefaultVersion {
		return -1
	}
	if b == DefaultVersion {
		return 1
	}
	if c := compareNatural(a, b); c != 0 {
		return c
	}
	// Naturally equivalent but not identical; break the tie bytewise so the
	// order stays total and antisymmetric.
	if a < b {
		return -1
	}
	return 1
}

// compareNatural compares a and b treating digit runs as numbers. It returns 0
// when the strings are equivalent under that rule, even if their spelling
// differs (leading zeros or an escaped separator).
func compareNatural(a, b string) int {
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		ad, bd := isDigit(a[ai]), isDigit(b[bi])
		switch {
		case ad && bd:
			ae, be := digitRunEnd(a, ai), digitRunEnd(b, bi)
			an := strings.TrimLeft(a[ai:ae], "0")
			bn := strings.TrimLeft(b[bi:be], "0")
			if len(an) != len(bn) {
				if len(an) < len(bn) {
					return -1
				}
				return 1
			}
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
			ai, bi = ae, be
		case ad != bd:
			if ad {
				return -1
			}
			return 1
		default:
			if a[ai] != b[bi] {
				if a[ai] < b[bi] {
					return -1
				}
				return 1
			}
			ai++
			bi++
		}
	}
	switch {
	case ai < len(a):
		return 1
	case bi < len(b):
		return -1
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func digitRunEnd(s string, start int) int {
	for start < len(s) && isDigit(s[start]) {
		start++
	}
	return start
}

// Registry is an explicitly versioned type registry.
//
// Registrations are copy-on-write. Snapshot captures the complete state at one
// instant as an immutable value, so a caller can keep marshaling and
// unmarshaling through one snapshot while other goroutines register, unload
// and re-freeze, then switch to a newer snapshot atomically with SetSnapshot.
// Registry is safe for concurrent use; all its marshal/unmarshal helpers pin a
// single snapshot for the duration of a call.
//
// The zero Registry is an empty registry. Use NewRegistry to name its default
// source.
type Registry struct {
	source Source

	mu       sync.Mutex
	entries  []Entry // immutable; replaced under mu
	frozen   bool
	selector SelectFunc

	current atomic.Pointer[Snapshot]
}

// Snapshot is an immutable view of a Registry. Lookups and codecs on a
// Snapshot always observe the same set of entries, even when the originating
// Registry is modified concurrently. The zero Snapshot is valid and empty.
type Snapshot struct {
	entries  []Entry // immutable
	selector SelectFunc

	byGoType map[reflect.Type][]Entry
	byWire   map[string]Entry
	byBase   map[string][]string
}

// NewRegistry returns an empty registry whose registrations are attributed to
// source by default.
func NewRegistry(source string) *Registry {
	return &Registry{
		source:   Source{Name: source},
		selector: DefaultSelector,
	}
}

// Source returns the default source name of the registry.
func (r *Registry) Source() Source { return r.source }

// SetSelector replaces the version selection rule used by future snapshots.
// A nil selector restores DefaultSelector. It returns ErrFrozen after Freeze.
func (r *Registry) SetSelector(fn SelectFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("set selector: %w", ErrFrozen)
	}
	if fn == nil {
		fn = DefaultSelector
	}
	r.selector = fn
	r.current.Store(nil)
	return nil
}

// Register adds e to the registry. Registrations are validated in full before
// being published; a failed Register leaves the registry unchanged.
//
// Register returns ErrConflict when:
//   - an entry already exists for the same type URL and version, even if it is
//     identical (a later hot-reload generation must not silently shadow an
//     earlier one; unregister the old source or switch snapshots instead),
//   - the same Go type is already registered under a different type URL (a Go
//     type maps to exactly one type URL, optionally at several versions), or
//   - two entries would encode to the same wire type URL.
//
// Register returns ErrFrozen after Freeze.
//
// A type URL must not contain VersionSeparator; that character is reserved for
// the version suffix.
func (r *Registry) Register(e Entry) error {
	if e.TypeURL == "" {
		return fmt.Errorf("register %v: empty type URL", e.GoType)
	}
	if strings.Contains(e.TypeURL, VersionSeparator) {
		return fmt.Errorf("register %q version %q: type URL must not contain %q, which separates the wire version",
			e.TypeURL, e.Version, VersionSeparator)
	}
	if e.GoType == nil {
		return fmt.Errorf("register %q version %q: nil Go type", e.TypeURL, e.Version)
	}
	if e.GoType.Kind() == reflect.Ptr {
		e.GoType = e.GoType.Elem()
	}
	if e.IsProto && !implementsProtoMessage(e.GoType) {
		return fmt.Errorf("register %q version %q: %s is marked as proto but does not implement proto.Message",
			e.TypeURL, e.Version, e.GoType)
	}
	if e.Source.Name == "" && e.Source.Detail == "" {
		e.Source = r.source
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("register %q version %q from %s: %w", e.TypeURL, e.Version, e.Source, ErrFrozen)
	}
	for _, old := range r.entries {
		switch {
		case old.TypeURL == e.TypeURL && old.Version == e.Version:
			return fmt.Errorf("register %q version %q from %s: already registered by %s: %w",
				e.TypeURL, e.Version, e.Source, old.Source, ErrConflict)
		case old.GoType == e.GoType && old.TypeURL != e.TypeURL:
			return fmt.Errorf("register type %s at %q version %q from %s: already registered at type URL %q version %q by %s: %w",
				e.GoType, e.TypeURL, e.Version, e.Source, old.TypeURL, old.Version, old.Source, ErrConflict)
		case old.WireTypeURL() == e.WireTypeURL():
			return fmt.Errorf("register %q version %q from %s: wire type URL collides with entry from %s: %w",
				e.TypeURL, e.Version, e.Source, old.Source, ErrConflict)
		}
	}
	entries := make([]Entry, len(r.entries), len(r.entries)+1)
	copy(entries, r.entries)
	r.entries = append(entries, e)
	r.current.Store(nil)
	return nil
}

// RegisterType registers the type of v under typeURL at version, choosing
// protocol buffer encoding when the type implements proto.Message (on either
// the value or pointer receiver) and JSON otherwise. v is normally a pointer.
func (r *Registry) RegisterType(v interface{}, typeURL string, version Version, source Source) error {
	t, err := typeOf(v)
	if err != nil {
		return err
	}
	return r.Register(Entry{
		TypeURL: typeURL,
		Version: version,
		GoType:  t,
		IsProto: implementsProtoMessage(t),
		Source:  source,
	})
}

// RegisterLegacy mirrors the package-level Register for a Registry: the URL is
// built with path.Join(args...), the type is encoded as JSON, and the entry is
// registered at DefaultVersion. Unlike Register, conflicts are returned as
// ErrConflict instead of panicking, and a prior URL for the same Go type is
// never silently replaced.
func (r *Registry) RegisterLegacy(v interface{}, args ...string) error {
	t, err := typeOf(v)
	if err != nil {
		return err
	}
	return r.Register(Entry{
		TypeURL: path.Join(args...),
		Version: DefaultVersion,
		GoType:  t,
		IsProto: false,
	})
}

// UnregisterSource removes every registration contributed by source and
// publishes a new snapshot. It returns the number of entries removed. It
// returns ErrFrozen after Freeze.
func (r *Registry) UnregisterSource(source Source) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return 0, fmt.Errorf("unregister source %s: %w", source, ErrFrozen)
	}
	kept := make([]Entry, 0, len(r.entries))
	removed := 0
	for _, e := range r.entries {
		if e.Source == source {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	if removed == 0 {
		return 0, nil
	}
	r.entries = kept
	r.current.Store(nil)
	return removed, nil
}

// Freeze prevents further registrations, unregistrations and selector
// changes. It publishes the snapshot of the final state. Freezing an already
// frozen registry is a no-op.
func (r *Registry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	r.frozen = true
	r.current.Store(r.buildLocked())
}

// Frozen reports whether the registry has been frozen.
func (r *Registry) Frozen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frozen
}

// Entries returns a copy of the registered entries in registration order.
func (r *Registry) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Entry(nil), r.entries...)
}

// Snapshot returns the current immutable snapshot, building and publishing one
// if the registry changed since it was last called. The snapshot is shared:
// callers must not modify it, but may use it concurrently and keep using it
// after the registry changes again.
func (r *Registry) Snapshot() *Snapshot {
	if s := r.current.Load(); s != nil {
		return s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.current.Load(); s != nil {
		return s
	}
	s := r.buildLocked()
	r.current.Store(s)
	return s
}

// SetSnapshot atomically publishes s as the registry's current snapshot and
// adopts its entries. This is how a running service switches codecs to a
// snapshot built elsewhere without ever mixing entries from two generations.
//
// SetSnapshot is allowed on a frozen registry: freezing stops registration
// churn, while switching snapshots remains the supported way to hot-swap a
// committed generation. A registry that was not frozen stays mutable and
// keeps its freeze state unchanged.
func (r *Registry) SetSnapshot(s *Snapshot) error {
	if s == nil {
		return errors.New("set snapshot: nil snapshot")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Copy the entries so a later Register cannot mutate the published
	// snapshot's backing array, and so the registry never aliases a Snapshot
	// owned by another Registry. Adopt the snapshot's selection rule too, so
	// the published view behaves exactly like the snapshot it replaces.
	r.entries = append([]Entry(nil), s.entries...)
	if s.selector != nil {
		r.selector = s.selector
	}
	r.current.Store(s)
	return nil
}

func (r *Registry) buildLocked() *Snapshot {
	return newSnapshot(r.entries, r.selector)
}

func newSnapshot(entries []Entry, selector SelectFunc) *Snapshot {
	if selector == nil {
		selector = DefaultSelector
	}
	s := &Snapshot{
		entries:  append([]Entry(nil), entries...),
		selector: selector,
		byGoType: make(map[reflect.Type][]Entry, len(entries)),
		byWire:   make(map[string]Entry, len(entries)),
		byBase:   make(map[string][]string),
	}
	for _, e := range entries {
		s.byGoType[e.GoType] = append(s.byGoType[e.GoType], e)
		s.byWire[e.WireTypeURL()] = e
		s.byBase[e.TypeURL] = append(s.byBase[e.TypeURL], string(e.Version))
	}
	// Sort both views with the same strict total order. Registration rejects
	// duplicate versions, so the sort is deterministic and the last element
	// is always the highest version.
	for t, es := range s.byGoType {
		sortEntries(es)
		s.byGoType[t] = es
	}
	for base, versions := range s.byBase {
		sort.Slice(versions, func(i, j int) bool {
			return CompareVersions(versions[i], versions[j]) < 0
		})
		s.byBase[base] = versions
	}
	return s
}

func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool {
		return CompareVersions(string(es[i].Version), string(es[j].Version)) < 0
	})
}

// Entries returns the entries captured by the snapshot, in registration order.
func (s *Snapshot) Entries() []Entry {
	if s == nil {
		return nil
	}
	return append([]Entry(nil), s.entries...)
}

// Versions returns the versions registered for the base type URL, sorted from
// lowest to highest (DefaultVersion first). typeURL may carry a version
// suffix; only its base is used.
func (s *Snapshot) Versions(typeURL string) []string {
	if s == nil {
		return nil
	}
	base, _ := DecodeTypeURL(typeURL)
	return append([]string(nil), s.byBase[base]...)
}

// Lookup returns the entry selected for the Go type of v. v may be a pointer
// or a value. When the same Go type is registered at several versions, the
// snapshot's selector chooses one deterministically. The bool result is false
// when the type is not registered.
func (s *Snapshot) Lookup(v interface{}) (Entry, bool) {
	if s == nil {
		return Entry{}, false
	}
	t, err := typeOf(v)
	if err != nil {
		return Entry{}, false
	}
	entries := s.byGoType[t]
	if len(entries) == 0 {
		return Entry{}, false
	}
	return s.selectEntry(entries)
}

// LookupVersion returns the entry for the Go type of v at exactly version.
func (s *Snapshot) LookupVersion(v interface{}, version Version) (Entry, bool) {
	if s == nil {
		return Entry{}, false
	}
	t, err := typeOf(v)
	if err != nil {
		return Entry{}, false
	}
	for _, e := range s.byGoType[t] {
		if e.Version == version {
			return e, true
		}
	}
	return Entry{}, false
}

func (s *Snapshot) selectEntry(entries []Entry) (Entry, bool) {
	if len(entries) == 0 {
		return Entry{}, false
	}
	versions := make([]string, len(entries))
	for i, e := range entries {
		versions[i] = string(e.Version)
	}
	want := s.selection(versions)
	for _, e := range entries {
		if string(e.Version) == want {
			return e, true
		}
	}
	// A misbehaving custom selector must not cause an arbitrary result: fall
	// back to the highest version.
	return entries[len(entries)-1], true
}

// selection applies the snapshot's selector, treating a zero Snapshot (nil
// selector) as DefaultSelector.
func (s *Snapshot) selection(versions []string) string {
	if s == nil || s.selector == nil {
		return DefaultSelector(versions)
	}
	return s.selector(versions)
}

// LookupTypeURL resolves typeURL to an entry.
//
// typeURL may be a wire URL carrying a version suffix. If it does, that
// version must be registered or ErrVersionMismatch is returned. If it does
// not, an explicit version argument selects that version exactly (including
// DefaultVersion, which asks for the unversioned registration); with no
// argument, the snapshot's selection rule chooses one. Unknown type URLs
// return ErrNotFound.
func (s *Snapshot) LookupTypeURL(typeURL string, version ...Version) (Entry, error) {
	if s == nil {
		return Entry{}, fmt.Errorf("type with url %q: %w", typeURL, ErrNotFound)
	}
	base, declared := DecodeTypeURL(typeURL)
	versions, ok := s.byBase[base]
	if !ok {
		return Entry{}, fmt.Errorf("type with url %q: %w", typeURL, ErrNotFound)
	}

	explicit := len(version) > 0
	want := string(declared)
	if explicit {
		if declared != DefaultVersion && declared != version[0] {
			return Entry{}, fmt.Errorf("type with url %q declares version %q but version %q was requested: %w",
				base, declared, version[0], ErrVersionMismatch)
		}
		want = string(version[0])
	} else if declared == DefaultVersion {
		want = s.selection(versions)
	}
	e, ok := s.byWire[EncodeTypeURL(base, Version(want))]
	if !ok {
		return Entry{}, fmt.Errorf("type with url %q version %q: known versions %v: %w",
			base, want, versions, ErrVersionMismatch)
	}
	return e, nil
}

// LookupWireURL resolves a wire type URL exactly as encoded in an Any.
func (s *Snapshot) LookupWireURL(wireTypeURL string) (Entry, error) {
	return s.LookupTypeURL(wireTypeURL)
}

// TypeURL returns the wire type URL for the Go type of v. When the type has
// several versions, the snapshot selection rule applies.
func (s *Snapshot) TypeURL(v interface{}) (string, error) {
	e, ok := s.Lookup(v)
	if !ok {
		return "", fmt.Errorf("type %s: %w", reflect.TypeOf(v), ErrNotFound)
	}
	return e.WireTypeURL(), nil
}

// TypeURLVersion returns the wire type URL for the Go type of v at version.
func (s *Snapshot) TypeURLVersion(v interface{}, version Version) (string, error) {
	e, ok := s.LookupVersion(v, version)
	if !ok {
		t, err := typeOf(v)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("type %s version %q: %w", t, version, ErrVersionMismatch)
	}
	return e.WireTypeURL(), nil
}

// MarshalAny marshals v into an Any using a single snapshot.
//
// proto.Message values registered with protocol buffer encoding are marshaled
// with proto; everything else is marshaled as JSON. An Any passed in is
// returned verbatim. Unregistered proto.Message values fall back to their
// protobuf full name, matching the package-level MarshalAny.
func (s *Snapshot) MarshalAny(v interface{}) (Any, error) {
	switch t := v.(type) {
	case Any:
		return t, nil
	case proto.Message:
		return s.marshalProto(t)
	default:
		return s.marshalJSON(v)
	}
}

// MarshalAnyVersion marshals v using exactly the entry registered at version.
// It returns ErrVersionMismatch when the Go type has no such registration.
func (s *Snapshot) MarshalAnyVersion(v interface{}, version Version) (Any, error) {
	if t, ok := v.(Any); ok {
		return t, nil
	}
	e, ok := s.LookupVersion(v, version)
	if !ok {
		t, err := typeOf(v)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("type %s version %q: %w", t, version, ErrVersionMismatch)
	}
	if !e.IsProto {
		return s.marshalJSONWithEntry(v, e)
	}
	pm, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("type %s registered as proto at %q version %q: value of type %T does not implement proto.Message (pass a pointer): %w",
			e.GoType, e.TypeURL, e.Version, v, ErrTypeMismatch)
	}
	return s.marshalProtoWithEntry(pm, e)
}

func (s *Snapshot) marshalProto(v proto.Message) (Any, error) {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	entries := s.byGoType[t]
	if len(entries) == 0 {
		// Unregistered: mirror the historical behavior of deriving the type
		// URL from the protobuf descriptor.
		wire := string(v.ProtoReflect().Descriptor().FullName())
		data, err := proto.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal %q: %w", wire, err)
		}
		return &anyType{typeURL: wire, value: data}, nil
	}
	e, _ := s.selectEntry(entries)
	if !e.IsProto {
		return s.marshalJSONWithEntry(v, e)
	}
	return s.marshalProtoWithEntry(v, e)
}

func (s *Snapshot) marshalProtoWithEntry(v proto.Message, e Entry) (Any, error) {
	data, err := proto.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal %q version %q: %w", e.TypeURL, e.Version, err)
	}
	return &anyType{typeURL: e.WireTypeURL(), value: data}, nil
}

func (s *Snapshot) marshalJSON(v interface{}) (Any, error) {
	e, ok := s.Lookup(v)
	if !ok {
		return nil, fmt.Errorf("type %s: %w", reflect.TypeOf(v), ErrNotFound)
	}
	if !e.IsProto {
		return s.marshalJSONWithEntry(v, e)
	}
	pm, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("type %s registered as proto at %q version %q: value of type %T does not implement proto.Message (pass a pointer): %w",
			e.GoType, e.TypeURL, e.Version, v, ErrTypeMismatch)
	}
	return s.marshalProtoWithEntry(pm, e)
}

func (s *Snapshot) marshalJSONWithEntry(v interface{}, e Entry) (Any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal %q version %q: %w", e.TypeURL, e.Version, err)
	}
	return &anyType{typeURL: e.WireTypeURL(), value: data}, nil
}

// UnmarshalAny decodes any into the Go type resolved by the snapshot.
func (s *Snapshot) UnmarshalAny(any Any) (interface{}, error) {
	if any == nil {
		return nil, errors.New("unmarshal: nil Any")
	}
	return s.UnmarshalByTypeURL(any.GetTypeUrl(), any.GetValue())
}

// UnmarshalByTypeURL decodes value through the entry selected for typeURL.
func (s *Snapshot) UnmarshalByTypeURL(typeURL string, value []byte) (interface{}, error) {
	e, err := s.LookupWireURL(typeURL)
	if err != nil {
		return nil, err
	}
	return s.decode(e, value, nil)
}

// UnmarshalByTypeURLVersion decodes value through the entry for typeURL at
// exactly version. If typeURL itself declares a different version,
// ErrVersionMismatch is returned.
func (s *Snapshot) UnmarshalByTypeURLVersion(typeURL string, version Version, value []byte) (interface{}, error) {
	e, err := s.LookupTypeURL(typeURL, version)
	if err != nil {
		return nil, err
	}
	return s.decode(e, value, nil)
}

// UnmarshalTo decodes any into out, which must be a pointer to the resolved
// type.
func (s *Snapshot) UnmarshalTo(any Any, out interface{}) error {
	if any == nil {
		return errors.New("unmarshal: nil Any")
	}
	return s.UnmarshalToByTypeURL(any.GetTypeUrl(), any.GetValue(), out)
}

// UnmarshalToByTypeURL decodes value into out.
func (s *Snapshot) UnmarshalToByTypeURL(typeURL string, value []byte, out interface{}) error {
	e, err := s.LookupWireURL(typeURL)
	if err != nil {
		return err
	}
	_, err = s.decode(e, value, out)
	return err
}

func (s *Snapshot) decode(e Entry, value []byte, out interface{}) (interface{}, error) {
	var v interface{}
	if out == nil {
		v = reflect.New(e.GoType).Interface()
	} else {
		if reflect.TypeOf(out) == nil || reflect.TypeOf(out).Kind() != reflect.Ptr {
			return nil, fmt.Errorf("unmarshal %q version %q into %T: output must be a pointer: %w",
				e.TypeURL, e.Version, out, ErrTypeMismatch)
		}
		t, err := typeOf(out)
		if err != nil {
			return nil, fmt.Errorf("unmarshal %q version %q into %T: %w", e.TypeURL, e.Version, out, ErrTypeMismatch)
		}
		if t != e.GoType {
			// Distinguish "right type, wrong version" from a plain type
			// mismatch; the former is actionable during a plugin upgrade.
			if entries, ok := s.byGoType[t]; ok {
				for _, other := range entries {
					if other.TypeURL == e.TypeURL {
						return nil, fmt.Errorf("unmarshal %q version %q (%s) into %s registered at version %q: %w",
							e.TypeURL, e.Version, e.GoType, t, other.Version, ErrVersionMismatch)
					}
				}
			}
			return nil, fmt.Errorf("unmarshal %q version %q (%s) into %s: %w",
				e.TypeURL, e.Version, e.GoType, t, ErrTypeMismatch)
		}
		v = out
	}

	if e.IsProto {
		pm, ok := v.(proto.Message)
		if !ok {
			return nil, fmt.Errorf("type %s registered as proto at %q version %q does not implement proto.Message: %w",
				e.GoType, e.TypeURL, e.Version, ErrTypeMismatch)
		}
		if err := proto.Unmarshal(value, pm); err != nil {
			return nil, fmt.Errorf("decode %q version %q: %w", e.TypeURL, e.Version, err)
		}
		return v, nil
	}
	if err := json.Unmarshal(value, v); err != nil {
		return nil, fmt.Errorf("decode %q version %q into %s: %w", e.TypeURL, e.Version, e.GoType, err)
	}
	return v, nil
}

// ResolveWire resolves a wire type URL through the snapshot, falling back to
// the protobuf global registry when the URL is unversioned and not explicitly
// registered. It returns ErrNotFound when neither knows the URL and
// ErrVersionMismatch when a versioned URL has no matching entry.
func (s *Snapshot) ResolveWire(wireTypeURL string) (Entry, error) {
	e, err := s.LookupWireURL(wireTypeURL)
	if err == nil {
		return e, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Entry{}, err
	}
	// A URL with a version suffix on a base URL that this snapshot knows is a
	// version gap, not an unknown type.
	base, version := DecodeTypeURL(wireTypeURL)
	if s != nil && len(s.byBase[base]) > 0 {
		return Entry{}, fmt.Errorf("type with url %q: known versions %v: %w",
			base, s.byBase[base], ErrVersionMismatch)
	}
	if version != DefaultVersion {
		return Entry{}, err
	}
	mt, ferr := protoregistry.GlobalTypes.FindMessageByURL(base)
	if ferr != nil {
		return Entry{}, fmt.Errorf("type with url %q: %w", wireTypeURL, ErrNotFound)
	}
	empty := mt.New().Interface()
	return Entry{
		TypeURL: base,
		Version: DefaultVersion,
		GoType:  reflect.TypeOf(empty).Elem(),
		IsProto: true,
		Source:  Source{Name: "protobuf"},
	}, nil
}

// Registry convenience methods. Each pins exactly one snapshot for the whole
// operation, so a concurrent SetSnapshot or registration can never cause a
// single Marshal or Unmarshal to mix two generations of the registry.

// TypeURL is Snapshot.TypeURL against the registry's current snapshot.
func (r *Registry) TypeURL(v interface{}) (string, error) {
	return r.Snapshot().TypeURL(v)
}

// MarshalAny is Snapshot.MarshalAny against the registry's current snapshot.
func (r *Registry) MarshalAny(v interface{}) (Any, error) {
	return r.Snapshot().MarshalAny(v)
}

// MarshalAnyVersion is Snapshot.MarshalAnyVersion against the current snapshot.
func (r *Registry) MarshalAnyVersion(v interface{}, version Version) (Any, error) {
	return r.Snapshot().MarshalAnyVersion(v, version)
}

// UnmarshalAny is Snapshot.UnmarshalAny against the registry's current
// snapshot.
func (r *Registry) UnmarshalAny(any Any) (interface{}, error) {
	return r.Snapshot().UnmarshalAny(any)
}

// UnmarshalByTypeURL is Snapshot.UnmarshalByTypeURL against the current
// snapshot.
func (r *Registry) UnmarshalByTypeURL(typeURL string, value []byte) (interface{}, error) {
	return r.Snapshot().UnmarshalByTypeURL(typeURL, value)
}

// UnmarshalByTypeURLVersion is Snapshot.UnmarshalByTypeURLVersion against the
// current snapshot.
func (r *Registry) UnmarshalByTypeURLVersion(typeURL string, version Version, value []byte) (interface{}, error) {
	return r.Snapshot().UnmarshalByTypeURLVersion(typeURL, version, value)
}

// UnmarshalTo is Snapshot.UnmarshalTo against the current snapshot.
func (r *Registry) UnmarshalTo(any Any, out interface{}) error {
	return r.Snapshot().UnmarshalTo(any, out)
}

// UnmarshalToByTypeURL is Snapshot.UnmarshalToByTypeURL against the current
// snapshot.
func (r *Registry) UnmarshalToByTypeURL(typeURL string, value []byte, out interface{}) error {
	return r.Snapshot().UnmarshalToByTypeURL(typeURL, value, out)
}

// typeOf returns the non-pointer type of v. Unlike tryDereference it returns
// an error instead of panicking, keeping registry lookups total.
func typeOf(v interface{}) (reflect.Type, error) {
	if v == nil {
		return nil, errors.New("nil value has no type")
	}
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t, nil
}

var protoMessageType = reflect.TypeOf((*proto.Message)(nil)).Elem()

// implementsProtoMessage reports whether t, or a pointer to t, implements
// proto.Message. Generated messages implement it on the pointer receiver, so
// both forms must be checked.
func implementsProtoMessage(t reflect.Type) bool {
	return t.Implements(protoMessageType) || reflect.PtrTo(t).Implements(protoMessageType)
}
