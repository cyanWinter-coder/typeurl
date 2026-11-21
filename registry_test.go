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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// globalURLSeq keeps package-global registry tests independent of each other
// under -count=N.
var globalURLSeq atomic.Uint64

// Versioned fixture types. Each version of a type URL is a distinct Go type,
// mirroring how a plugin reload brings in a new struct definition for the same
// logical message.

type greetingV1 struct {
	Name string `json:"name"`
}

type greetingV2 struct {
	Name string `json:"name"`
	Lang string `json:"lang"`
}

type greetingV10 struct {
	Name string `json:"name"`
	Lang string `json:"lang"`
	Mood string `json:"mood"`
}

type unrelatedType struct {
	Value int `json:"value"`
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry("test")
}

func mustRegister(t *testing.T, r *Registry, e Entry) {
	t.Helper()
	if err := r.Register(e); err != nil {
		t.Fatalf("register %v: %v", e, err)
	}
}

func TestEntryRecordsFields(t *testing.T) {
	r := newTestRegistry(t)
	src := Source{Name: "plugin-greeting", Detail: "build-42"}
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{}), IsProto: false, Source: src})

	entries := r.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.TypeURL != "example.com/greeting" || e.Version != "v2" {
		t.Fatalf("unexpected identity: %q %q", e.TypeURL, e.Version)
	}
	if e.GoType != reflect.TypeOf(greetingV2{}) {
		t.Fatalf("unexpected Go type: %v", e.GoType)
	}
	if e.Source != src {
		t.Fatalf("unexpected source: %v", e.Source)
	}
	if got, want := e.WireTypeURL(), "example.com/greeting#v2"; got != want {
		t.Fatalf("wire url %q != %q", got, want)
	}

	// Pointer types are normalized to their element type.
	mustRegister(t, r, Entry{TypeURL: "example.com/other", GoType: reflect.TypeOf(&greetingV1{})})
	if got := r.Entries()[1].GoType; got != reflect.TypeOf(greetingV1{}) {
		t.Fatalf("pointer not dereferenced: %v", got)
	}
}

func TestRegisterDefaultSource(t *testing.T) {
	r := NewRegistry("package-x")
	mustRegister(t, r, Entry{TypeURL: "example.com/x", GoType: reflect.TypeOf(greetingV1{})})
	if got := r.Entries()[0].Source; got.Name != "package-x" {
		t.Fatalf("unexpected default source: %v", got)
	}
}

func TestDuplicateVersionConflicts(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{}), Source: Source{Name: "a"}})

	t.Run("identical entry", func(t *testing.T) {
		// Even an identical entry conflicts: a second plugin generation must
		// not silently take over the registration.
		err := r.Register(Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{}), Source: Source{Name: "a"}})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected ErrConflict, got %v", err)
		}
	})

	t.Run("same url version different type", func(t *testing.T) {
		err := r.Register(Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV2{}), Source: Source{Name: "b"}})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected ErrConflict, got %v", err)
		}
	})

	t.Run("same type different url", func(t *testing.T) {
		err := r.Register(Entry{TypeURL: "example.com/other", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected ErrConflict, got %v", err)
		}
	})

	t.Run("same type same url new version", func(t *testing.T) {
		mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV1{})})
	})

	t.Run("separator in url", func(t *testing.T) {
		err := r.Register(Entry{TypeURL: "example.com/bad#url", GoType: reflect.TypeOf(unrelatedType{})})
		if err == nil {
			t.Fatal("expected error for type URL containing the version separator")
		}
	})

	// A failed registration must leave the registry unchanged.
	if got := len(r.Entries()); got != 2 {
		t.Fatalf("failed registrations mutated the registry: %d entries", got)
	}
}

func TestWireEncodingCompatibility(t *testing.T) {
	for _, tc := range []struct {
		base    string
		version Version
		wire    string
	}{
		{"example.com/foo", DefaultVersion, "example.com/foo"},
		{"example.com/foo", "v1", "example.com/foo#v1"},
		{"example.com/foo", "1.0.0", "example.com/foo#1.0.0"},
		{"example.com/foo", "v1#draft", "example.com/foo#v1%23draft"},
		{"example.com/foo", "a/b c", "example.com/foo#a%2Fb%20c"},
	} {
		wire := EncodeTypeURL(tc.base, tc.version)
		if wire != tc.wire {
			t.Fatalf("EncodeTypeURL(%q, %q) = %q, want %q", tc.base, tc.version, wire, tc.wire)
		}
		base, version := DecodeTypeURL(wire)
		if base != tc.base || version != tc.version {
			t.Fatalf("DecodeTypeURL(%q) = (%q, %q), want (%q, %q)", wire, base, version, tc.base, tc.version)
		}
	}

	// The default version must stay byte-for-byte identical to the historical
	// URL strings produced by the package-level Register.
	if got := EncodeTypeURL("types.containerd.io/opencontainers/runtime-spec/1/Spec", DefaultVersion); got != "types.containerd.io/opencontainers/runtime-spec/1/Spec" {
		t.Fatalf("default version changed the wire URL: %q", got)
	}
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{DefaultVersion, "v1", "v2", "v9", "v10", "v10.1", "v11"}
	for i := 0; i < len(ordered); i++ {
		for j := 0; j < len(ordered); j++ {
			got := CompareVersions(ordered[i], ordered[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if (got < 0) != (want < 0) || (got > 0) != (want > 0) || (got == 0) != (want == 0) {
				t.Fatalf("CompareVersions(%q, %q) = %d, want sign %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	// Leading zeros are distinct registration keys but must still order
	// totally and consistently.
	if CompareVersions("v01", "v1") == 0 {
		t.Fatal("distinct versions must not compare equal")
	}
	if a, b := CompareVersions("v01", "v1"), CompareVersions("v1", "v01"); a != -b {
		t.Fatalf("comparison is not antisymmetric: %d vs %d", a, b)
	}
}

func TestDefaultSelectionIsDeterministic(t *testing.T) {
	// The same set of registrations in two different orders must select the
	// same version.
	entries := []Entry{
		{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})},
		{TypeURL: "example.com/greeting", Version: "v10", GoType: reflect.TypeOf(greetingV10{})},
		{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})},
	}

	build := func(order []Entry) *Snapshot {
		r := newTestRegistry(t)
		for _, e := range order {
			mustRegister(t, r, e)
		}
		return r.Snapshot()
	}

	forward := build(entries)
	reversed := build([]Entry{entries[2], entries[1], entries[0]})

	for _, s := range []*Snapshot{forward, reversed} {
		e, err := s.LookupTypeURL("example.com/greeting")
		if err != nil {
			t.Fatal(err)
		}
		if e.Version != "v10" {
			t.Fatalf("expected highest version v10, got %q", e.Version)
		}
	}

	// Adding the legacy default version takes precedence over all versioned
	// entries, matching the historical behavior.
	legacyDefault := Entry{TypeURL: "example.com/greeting", GoType: reflect.TypeOf(greetingV10{})}
	withDefault := build(append([]Entry{legacyDefault}, entries...))
	e, err := withDefault.LookupTypeURL("example.com/greeting")
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != DefaultVersion {
		t.Fatalf("expected default version to win, got %q", e.Version)
	}
}

func TestLookupByDeclaredVersion(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
	s := r.Snapshot()

	e, err := s.LookupTypeURL("example.com/greeting#v1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != "v1" || e.GoType != reflect.TypeOf(greetingV1{}) {
		t.Fatalf("declared version ignored: %v", e)
	}

	if _, err := s.LookupTypeURL("example.com/greeting#v3"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}

	// Caller-selected version.
	e, err = s.LookupTypeURL("example.com/greeting", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != "v1" {
		t.Fatalf("caller selection ignored: %v", e)
	}

	// Caller selection contradicting the declared version.
	if _, err := s.LookupTypeURL("example.com/greeting#v2", "v1"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}

	// Unknown base URL.
	if _, err := s.LookupTypeURL("example.com/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDefaultVersionPrecedence(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: DefaultVersion, GoType: reflect.TypeOf(greetingV1{})})

	s := r.Snapshot()
	e, err := s.LookupTypeURL("example.com/greeting")
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != DefaultVersion {
		t.Fatalf("expected default version to win, got %q", e.Version)
	}
	// The legacy spelling remains valid and unchanged.
	if got := e.WireTypeURL(); got != "example.com/greeting" {
		t.Fatalf("unexpected wire url %q", got)
	}
}

func TestCustomSelector(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.SetSelector(func(versions []string) string {
		// Always pick the oldest non-default version, for a
		// "compatibility-first" deployment.
		for _, v := range versions {
			if v != DefaultVersion {
				return v
			}
		}
		return DefaultVersion
	}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v10", GoType: reflect.TypeOf(greetingV10{})})

	e, err := r.Snapshot().LookupTypeURL("example.com/greeting")
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != "v2" {
		t.Fatalf("custom selector ignored, got %q", e.Version)
	}

	// A declared version is resolved exactly and never goes through the
	// selector.
	e, err = r.Snapshot().LookupTypeURL("example.com/greeting#v10")
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != "v10" {
		t.Fatalf("declared version routed through selector: %q", e.Version)
	}
}

func TestSnapshotIsolation(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})

	before := r.Snapshot()
	if before != r.Snapshot() {
		t.Fatal("Snapshot must return the cached snapshot until the registry changes")
	}

	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
	after := r.Snapshot()
	if before == after {
		t.Fatal("registry change must publish a new snapshot")
	}

	// The old snapshot keeps decoding v1 only.
	if _, err := before.LookupTypeURL("example.com/greeting#v2"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("old snapshot observed a new registration: %v", err)
	}
	if _, err := after.LookupTypeURL("example.com/greeting#v2"); err != nil {
		t.Fatalf("new snapshot missing registration: %v", err)
	}

	// Atomically switching back makes the registry serve the old view again.
	if err := r.SetSnapshot(before); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot() != before {
		t.Fatal("SetSnapshot did not publish the supplied snapshot")
	}
	if _, err := r.UnmarshalByTypeURL("example.com/greeting#v2", []byte(`{}`)); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("registry still serves removed version: %v", err)
	}
}

func TestFreeze(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	r.Freeze()
	if !r.Frozen() {
		t.Fatal("expected registry to be frozen")
	}

	if err := r.Register(Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("expected ErrFrozen, got %v", err)
	}
	if _, err := r.UnregisterSource(Source{Name: "test"}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("expected ErrFrozen from UnregisterSource, got %v", err)
	}
	if err := r.SetSelector(nil); !errors.Is(err, ErrFrozen) {
		t.Fatalf("expected ErrFrozen from SetSelector, got %v", err)
	}
	// Freezing twice is a no-op.
	r.Freeze()

	// The frozen snapshot is still usable and stable.
	s := r.Snapshot()
	if s != r.Snapshot() {
		t.Fatal("frozen registry must serve a stable snapshot")
	}
	if _, err := s.UnmarshalByTypeURL("example.com/greeting", []byte(`{"name":"x"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestUnregisterSource(t *testing.T) {
	r := newTestRegistry(t)
	pluginA := Source{Name: "plugin-a", Detail: "gen1"}
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{}), Source: pluginA})
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v10", GoType: reflect.TypeOf(greetingV10{}), Source: pluginA})
	mustRegister(t, r, Entry{TypeURL: "example.com/other", GoType: reflect.TypeOf(unrelatedType{}), Source: Source{Name: "core"}})

	old := r.Snapshot()

	n, err := r.UnregisterSource(pluginA)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 entries removed, got %d", n)
	}
	// The pre-unload snapshot remains valid.
	if _, err := old.LookupTypeURL("example.com/greeting#v1"); err != nil {
		t.Fatalf("live snapshot mutated by unload: %v", err)
	}
	if _, err := r.Snapshot().LookupTypeURL("example.com/greeting#v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unloaded entry still resolvable: %v", err)
	}
	if _, err := r.Snapshot().LookupTypeURL("example.com/other"); err != nil {
		t.Fatalf("unrelated registration lost: %v", err)
	}

	// Unloading an unknown source is a no-op.
	n, err = r.UnregisterSource(Source{Name: "never-loaded"})
	if err != nil || n != 0 {
		t.Fatalf("unexpected result for unknown source: %d %v", n, err)
	}
}

func TestMarshalUnmarshalVersionsJSON(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})

	s := r.Snapshot()

	a, err := s.MarshalAnyVersion(&greetingV1{Name: "bob"}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.GetTypeUrl(), "example.com/greeting#v1"; got != want {
		t.Fatalf("type url %q != %q", got, want)
	}

	v, err := s.UnmarshalAny(a)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.(*greetingV1); !ok || got.Name != "bob" {
		t.Fatalf("unexpected decode: %#v", v)
	}

	a2, err := s.MarshalAnyVersion(&greetingV2{Name: "bob", Lang: "en"}, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a2.GetTypeUrl(), "example.com/greeting#v2"; got != want {
		t.Fatalf("type url %q != %q", got, want)
	}
	v, err = s.UnmarshalAny(a2)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.(*greetingV2); !ok || got.Lang != "en" {
		t.Fatalf("unexpected decode: %#v", v)
	}

	// The default selection uses the highest version when no default exists.
	a3, err := s.MarshalAny(&greetingV2{Name: "bob", Lang: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a3.GetTypeUrl(), "example.com/greeting#v2"; got != want {
		t.Fatalf("default selection type url %q != %q", got, want)
	}

	// UnmarshalTo with the matching type succeeds.
	var out greetingV1
	if err := s.UnmarshalTo(a, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "bob" {
		t.Fatalf("unexpected output: %#v", out)
	}
}

func TestUnmarshalErrors(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
	mustRegister(t, r, Entry{TypeURL: "example.com/other", GoType: reflect.TypeOf(unrelatedType{})})
	s := r.Snapshot()

	t.Run("unknown url", func(t *testing.T) {
		_, err := s.UnmarshalByTypeURL("example.com/unknown", []byte(`{}`))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("unknown version", func(t *testing.T) {
		_, err := s.UnmarshalByTypeURL("example.com/greeting#v9", []byte(`{}`))
		if !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("expected ErrVersionMismatch, got %v", err)
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		err := s.UnmarshalToByTypeURL("example.com/greeting#v1", []byte(`{"name":"x"}`), &unrelatedType{})
		if !errors.Is(err, ErrTypeMismatch) {
			t.Fatalf("expected ErrTypeMismatch, got %v", err)
		}
	})

	t.Run("wrong version type", func(t *testing.T) {
		// greetingV2 is registered at the same type URL but v2.
		err := s.UnmarshalToByTypeURL("example.com/greeting#v1", []byte(`{"name":"x","lang":"en"}`), &greetingV2{})
		if !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("expected ErrVersionMismatch, got %v", err)
		}
	})

	t.Run("bad payload", func(t *testing.T) {
		_, err := s.UnmarshalByTypeURL("example.com/greeting#v1", []byte(`{"name":`))
		if err == nil {
			t.Fatal("expected decode error")
		}
	})

	t.Run("version contradictory to caller", func(t *testing.T) {
		_, err := s.UnmarshalByTypeURLVersion("example.com/greeting#v2", "v1", []byte(`{}`))
		if !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("expected ErrVersionMismatch, got %v", err)
		}
	})
}

func TestMarshalUnmarshalVersionsProto(t *testing.T) {
	r := newTestRegistry(t)
	const url = "example.com/timestamp"
	mustRegister(t, r, Entry{TypeURL: url, Version: "v1", GoType: reflect.TypeOf(timestamppb.Timestamp{}), IsProto: true})
	mustRegister(t, r, Entry{TypeURL: url, Version: "v2", GoType: reflect.TypeOf(timestamppb.Timestamp{}), IsProto: true})

	s := r.Snapshot()
	now := time.Now()

	a, err := s.MarshalAnyVersion(timestamppb.New(now), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.GetTypeUrl(), url+"#v1"; got != want {
		t.Fatalf("type url %q != %q", got, want)
	}

	v, err := s.UnmarshalAny(a)
	if err != nil {
		t.Fatal(err)
	}
	ts, ok := v.(*timestamppb.Timestamp)
	if !ok {
		t.Fatalf("unexpected type %T", v)
	}
	if !ts.AsTime().Equal(now.Truncate(time.Second)) && ts.AsTime().Sub(now) != 0 {
		t.Fatalf("unexpected time: %v != %v", ts.AsTime(), now)
	}

	// Unregistered proto messages still fall back to the descriptor-derived
	// type URL, as the package-level API always did.
	other := timestamppb.New(now)
	a2, err := r.MarshalAny(other)
	if err != nil {
		t.Fatal(err)
	}
	// timestamppb.Timestamp is registered in this registry, so the fallback
	// does not trigger; a registry without the entry exercises it.
	empty := newTestRegistry(t).Snapshot()
	a3, err := empty.MarshalAny(other)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a3.GetTypeUrl(), "google.protobuf.Timestamp"; got != want {
		t.Fatalf("fallback type url %q != %q", got, want)
	}
	if a2.GetTypeUrl() == "" {
		t.Fatal("registered timestamp lost its type url")
	}

	// proto payloads survive a round trip through the compatibility Any type.
	pb := MarshalProto(a)
	if pb.GetTypeUrl() != a.GetTypeUrl() {
		t.Fatalf("MarshalProto changed the type url: %q", pb.GetTypeUrl())
	}
	if err := proto.Unmarshal(pb.GetValue(), &timestamppb.Timestamp{}); err != nil {
		t.Fatalf("invalid proto payload: %v", err)
	}
}

func TestSnapshotSeparationOfProtoAndJSON(t *testing.T) {
	// A type registered through the legacy JSON path must keep decoding as
	// JSON even though it implements proto.Message. This mirrors the
	// historical Register semantics.
	r := newTestRegistry(t)
	if err := r.RegisterLegacy(&timestamppb.Timestamp{}, "example.com/legacy-ts"); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()

	e, err := s.LookupTypeURL("example.com/legacy-ts")
	if err != nil {
		t.Fatal(err)
	}
	if e.IsProto {
		t.Fatal("legacy registration must use JSON")
	}

	a, err := s.MarshalAny(&timestamppb.Timestamp{Seconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.GetValue()) == 0 || a.GetValue()[0] != '{' {
		t.Fatalf("expected JSON payload, got %q", a.GetValue())
	}
	v, err := s.UnmarshalAny(a)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(*timestamppb.Timestamp); got.Seconds != 5 {
		t.Fatalf("unexpected value: %v", got)
	}
}

func TestGlobalRegistryCompatibility(t *testing.T) {
	// The legacy global table is untouched by the versioned API.
	type globalLegacy struct {
		N int `json:"n"`
	}
	Register(&globalLegacy{}, "example.com/global-legacy")

	any, err := MarshalAny(&globalLegacy{N: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := any.GetTypeUrl(), "example.com/global-legacy"; got != want {
		t.Fatalf("legacy url %q != %q", got, want)
	}
	v, err := UnmarshalAny(any)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.(*globalLegacy); !ok || got.N != 3 {
		t.Fatalf("unexpected legacy decode: %#v", v)
	}

	// A versioned registration on the default registry is visible through
	// the package-level helpers, and marshals with its version suffix.
	type globalVersioned struct {
		S string `json:"s"`
	}
	// The default registry is process-global, so use a unique URL per test
	// invocation to stay correct under -count=N.
	url := fmt.Sprintf("example.com/global-versioned/%d", globalURLSeq.Add(1))
	src := Source{Name: "global-test"}
	if err := RegisterVersioned(&globalVersioned{}, url, "v2", src); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = UnregisterSource(src) })
	a, err := MarshalAny(&globalVersioned{S: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.GetTypeUrl(), url+"#v2"; got != want {
		t.Fatalf("versioned url %q != %q", got, want)
	}
	v, err = UnmarshalAny(a)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v.(*globalVersioned); !ok || got.S != "x" {
		t.Fatalf("unexpected versioned decode: %#v", v)
	}

	// The global protobuf fallback still works for unversioned URLs.
	b, err := proto.Marshal(timestamppb.New(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalByTypeURL("type.googleapis.com/google.protobuf.Timestamp", b); err != nil {
		t.Fatal(err)
	}

	// A versioned URL is never served by the protobuf fallback.
	if _, err := UnmarshalByTypeURL("google.protobuf.Timestamp#v9", b); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a versioned global URL, got %v", err)
	}
}

func TestDefaultRegistryFreezeDoesNotAffectLegacyRegister(t *testing.T) {
	// Freezing a user-created registry never touches the package globals.
	r := newTestRegistry(t)
	r.Freeze()

	type stillWorks struct {
		V int
	}
	Register(&stillWorks{}, "example.com/still-works")
	if _, err := TypeURL(&stillWorks{}); err != nil {
		t.Fatalf("legacy registry affected by unrelated freeze: %v", err)
	}
}

func TestConcurrentRegisterFreezeSwitchUnload(t *testing.T) {
	r := newTestRegistry(t)
	const base = 1000

	var wg sync.WaitGroup
	start := make(chan struct{})

	// Registrations of distinct type URLs from several "plugins".
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			<-start
			for i := 0; i < base/4; i++ {
				url := fmt.Sprintf("example.com/p%d/t%d", p, i)
				_ = r.Register(Entry{
					TypeURL: url,
					Version: "v1",
					GoType:  reflect.TypeOf(greetingV1{}),
					Source:  Source{Name: fmt.Sprintf("plugin-%d", p)},
				})
			}
		}(p)
	}

	// Snapshot readers and switchers.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < base; i++ {
				s := r.Snapshot()
				if s == nil {
					t.Error("nil snapshot")
					return
				}
				_, _ = s.LookupTypeURL("example.com/p0/t0")
				_, _ = s.UnmarshalByTypeURL("example.com/p1/t1", []byte(`{"name":"x"}`))
				if i%97 == 0 {
					_ = r.SetSnapshot(s)
				}
			}
		}(w)
	}

	// Unloaders that repeatedly remove and re-add their source.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		src := Source{Name: "plugin-churn"}
		for i := 0; i < 200; i++ {
			_ = r.Register(Entry{TypeURL: "example.com/churn", Version: Version(fmt.Sprintf("v%d", i)), GoType: reflect.TypeOf(greetingV1{}), Source: src})
			_, _ = r.UnregisterSource(src)
			_ = r.Snapshot()
		}
	}()

	close(start)
	wg.Wait()

	// The registry remains internally consistent after the storm.
	s := r.Snapshot()
	if len(s.Entries()) == 0 {
		t.Fatal("all registrations lost")
	}
	if _, err := s.LookupTypeURL("example.com/churn"); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("inconsistent churn entry: %v", err)
	}
}

func TestConcurrentFrozenReads(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	r.Freeze()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				if _, err := r.UnmarshalByTypeURL("example.com/greeting", []byte(`{"name":"x"}`)); err != nil {
					t.Error(err)
					return
				}
				_ = r.Register(Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
			}
		}()
	}
	wg.Wait()
}

func TestGlobalVersionMismatchIsExplicit(t *testing.T) {
	type globalVersionedMismatch struct {
		S string `json:"s"`
	}
	url := fmt.Sprintf("example.com/global-mismatch/%d", globalURLSeq.Add(1))
	src := Source{Name: "global-mismatch-test"}
	if err := RegisterVersioned(&globalVersionedMismatch{}, url, "v2", src); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = UnregisterSource(src) })

	// A known type URL with an unregistered version must report the version
	// problem, not a generic "not found", so operators can tell a stale
	// plugin from an unknown type. An unknown base URL still reports
	// ErrNotFound.
	if _, err := UnmarshalByTypeURL(url+"#v3", []byte(`{}`)); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}
	if _, err := UnmarshalByTypeURL(url+"-unknown", []byte(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown base URL, got %v", err)
	}
}

func TestSnapshotConvenienceMethods(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
	s := r.Snapshot()

	if got, want := s.Versions("example.com/greeting#v1"), []string{"v1", "v2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Versions = %v, want %v", got, want)
	}
	if got := s.Versions("example.com/missing"); len(got) != 0 {
		t.Fatalf("Versions for unknown url = %v", got)
	}

	if e, ok := s.LookupVersion(&greetingV1{}, "v1"); !ok || e.Version != "v1" {
		t.Fatalf("LookupVersion failed: %v %v", e, ok)
	}
	if _, ok := s.LookupVersion(&greetingV1{}, "v2"); ok {
		t.Fatal("LookupVersion matched the wrong type")
	}
	if e, ok := s.Lookup(&greetingV2{}); !ok || e.Version != "v2" {
		t.Fatalf("Lookup failed: %v %v", e, ok)
	}

	if got, err := s.TypeURLVersion(&greetingV1{}, "v1"); err != nil || got != "example.com/greeting#v1" {
		t.Fatalf("TypeURLVersion = %q, %v", got, err)
	}
	if _, err := s.TypeURLVersion(&greetingV1{}, "v9"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}
	if got, err := r.TypeURL(&greetingV2{}); err != nil || got != "example.com/greeting#v2" {
		t.Fatalf("Registry.TypeURL = %q, %v", got, err)
	}

	// Explicitly asking for the default version must not silently fall back
	// to a versioned registration.
	if _, err := s.LookupTypeURL("example.com/greeting", DefaultVersion); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch for missing default version, got %v", err)
	}
}

func TestMarshalErrorsAreExplicit(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	s := r.Snapshot()

	if _, err := s.MarshalAny(&unrelatedType{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := s.MarshalAnyVersion(&greetingV1{}, "v2"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}
	if _, err := s.MarshalAnyVersion(&unrelatedType{}, "v1"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}
}

func TestSetSnapshotAfterFreeze(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	old := r.Snapshot()
	r.Freeze()

	other := newTestRegistry(t)
	mustRegister(t, other, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
	fresh := other.Snapshot()

	if err := r.SetSnapshot(fresh); err != nil {
		t.Fatalf("SetSnapshot on frozen registry: %v", err)
	}
	if r.Snapshot() != fresh {
		t.Fatal("snapshot not swapped")
	}
	if _, err := r.UnmarshalByTypeURL("example.com/greeting#v2", []byte(`{"name":"a","lang":"en"}`)); err != nil {
		t.Fatalf("new snapshot not usable: %v", err)
	}
	if _, err := old.LookupTypeURL("example.com/greeting#v2"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("previous snapshot changed: %v", err)
	}
	// The frozen registry is still frozen for registration.
	if err := r.Register(Entry{TypeURL: "example.com/other", GoType: reflect.TypeOf(unrelatedType{})}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("expected ErrFrozen, got %v", err)
	}
	if err := r.SetSnapshot(nil); err == nil {
		t.Fatal("expected an error for a nil snapshot")
	}
}

func TestUnregisterSourceRequiresExactMatch(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/a", GoType: reflect.TypeOf(greetingV1{}), Source: Source{Name: "plugin", Detail: "1"}})
	mustRegister(t, r, Entry{TypeURL: "example.com/b", GoType: reflect.TypeOf(unrelatedType{}), Source: Source{Name: "plugin", Detail: "2"}})

	if n, err := r.UnregisterSource(Source{Name: "plugin"}); err != nil || n != 0 {
		t.Fatalf("partial source match removed entries: %d, %v", n, err)
	}
	if n, err := r.UnregisterSource(Source{Name: "plugin", Detail: "1"}); err != nil || n != 1 {
		t.Fatalf("exact source match failed: %d, %v", n, err)
	}
	if _, err := r.Snapshot().LookupTypeURL("example.com/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entry from removed source still present: %v", err)
	}
	if _, err := r.Snapshot().LookupTypeURL("example.com/b"); err != nil {
		t.Fatalf("entry from other source lost: %v", err)
	}
}

func TestConcurrentSnapshotPinning(t *testing.T) {
	// A snapshot held by a reader must never observe entries added after it
	// was taken, even while another goroutine switches the registry.
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	pinned := r.Snapshot()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 2; i < 50; i++ {
			_ = r.Register(Entry{
				TypeURL: "example.com/greeting",
				Version: Version(fmt.Sprintf("v%d", i)),
				GoType:  reflect.TypeOf(greetingV1{}),
			})
			_ = r.SetSnapshot(r.Snapshot())
		}
	}()

	for i := 0; i < 1000; i++ {
		if got := pinned.Versions("example.com/greeting"); !reflect.DeepEqual(got, []string{"v1"}) {
			t.Fatalf("pinned snapshot changed: %v", got)
		}
	}
	<-done
	if got := len(r.Snapshot().Versions("example.com/greeting")); got < 2 {
		t.Fatalf("registry did not gain versions: %d", got)
	}
}

func TestRegistryConvenienceMethods(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v2", GoType: reflect.TypeOf(greetingV2{})})
	RegisterVersioned(&greetingV1{}, "example.com/greeting", "v1", Source{Name: "convenience"})

	a, err := r.MarshalAnyVersion(&greetingV1{Name: "x"}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.UnmarshalAny(a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UnmarshalByTypeURL("example.com/greeting#v2", []byte(`{"name":"x","lang":"en"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UnmarshalByTypeURLVersion("example.com/greeting", "v2", []byte(`{"lang":"en"}`)); err != nil {
		t.Fatal(err)
	}
	var out greetingV2
	if err := r.UnmarshalTo(a2Any("example.com/greeting#v2", `{"lang":"en"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Lang != "en" {
		t.Fatalf("unexpected output: %#v", out)
	}
	if err := r.UnmarshalToByTypeURL("example.com/greeting#v1", []byte(`{"name":"y"}`), &greetingV1{}); err != nil {
		t.Fatal(err)
	}
}

type rawAny struct {
	url   string
	value []byte
}

func (a rawAny) GetTypeUrl() string { return a.url }
func (a rawAny) GetValue() []byte   { return a.value }

func a2Any(url, value string) Any { return rawAny{url: url, value: []byte(value)} }

func TestResolveWire(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1", GoType: reflect.TypeOf(greetingV1{})})
	s := r.Snapshot()

	e, err := s.ResolveWire("example.com/greeting#v1")
	if err != nil || e.Version != "v1" {
		t.Fatalf("ResolveWire did not use the versioned entry: %v %v", e, err)
	}

	// Unversioned protobuf messages fall back to the global registry...
	if _, err := s.ResolveWire("google.protobuf.Timestamp"); err != nil {
		t.Fatalf("global fallback failed: %v", err)
	}
	// ...but a versioned URL is never served by it. An unknown base URL with a
	// version suffix has no known versions, so it is still ErrNotFound.
	if _, err := s.ResolveWire("google.protobuf.Timestamp#v2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// A known base URL at an unknown version reports the version gap.
	if _, err := s.ResolveWire("example.com/greeting#v9"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}
	// Truly unknown URLs are ErrNotFound.
	if _, err := s.ResolveWire("example.com/does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// A known versioned URL with an unknown version reports the version gap.
	if _, err := s.ResolveWire("example.com/greeting#v9"); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}
}

func TestDefaultRegistryAccessor(t *testing.T) {
	if DefaultRegistry() == nil {
		t.Fatal("DefaultRegistry returned nil")
	}
	if DefaultRegistry().Source().Name != "typeurl" {
		t.Fatalf("unexpected default source: %v", DefaultRegistry().Source())
	}
}

func TestVersionedProtoRequiresProtoMessage(t *testing.T) {
	r := newTestRegistry(t)
	err := r.Register(Entry{TypeURL: "example.com/bad", GoType: reflect.TypeOf(greetingV1{}), IsProto: true})
	if err == nil {
		t.Fatal("expected an error registering a non-proto type as proto")
	}

	// A proto entry marshaled with a non-pointer value reports ErrTypeMismatch.
	mustRegister(t, r, Entry{TypeURL: "example.com/ts", Version: "v1", GoType: reflect.TypeOf(timestamppb.Timestamp{}), IsProto: true})
	if _, err := r.MarshalAnyVersion(timestamppb.Timestamp{}, "v1"); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
}

func TestVersionContainingSeparatorRoundTrips(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, Entry{TypeURL: "example.com/greeting", Version: "v1#draft", GoType: reflect.TypeOf(greetingV1{})})

	s := r.Snapshot()
	e, err := s.LookupTypeURL(EncodeTypeURL("example.com/greeting", "v1#draft"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Version != "v1#draft" {
		t.Fatalf("unexpected version %q", e.Version)
	}

	a, err := s.MarshalAnyVersion(&greetingV1{Name: "x"}, "v1#draft")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.GetTypeUrl(), "example.com/greeting#v1%23draft"; got != want {
		t.Fatalf("wire url %q != %q", got, want)
	}
	v, err := s.UnmarshalAny(a)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(*greetingV1); got.Name != "x" {
		t.Fatalf("unexpected decode: %#v", got)
	}
}

func FuzzEncodeDecodeTypeURL(f *testing.F) {
	f.Add("example.com/foo", "v1")
	f.Add("example.com/foo", DefaultVersion)
	f.Add("example.com/foo", "v1#draft")
	f.Add("example.com/foo", "a/b c")
	f.Add("", "x")
	f.Fuzz(func(t *testing.T, base, version string) {
		if strings.Contains(base, VersionSeparator) {
			// A base containing the separator is ambiguous by construction
			// and is rejected at registration time.
			return
		}
		wire := EncodeTypeURL(base, Version(version))
		gotBase, gotVersion := DecodeTypeURL(wire)
		if version == DefaultVersion {
			if gotBase != base || gotVersion != DefaultVersion {
				t.Fatalf("default version round trip: (%q, %q) != (%q, %q)", gotBase, gotVersion, base, DefaultVersion)
			}
			return
		}
		if gotBase != base || string(gotVersion) != version {
			t.Fatalf("round trip %q %q: got (%q, %q)", base, version, gotBase, gotVersion)
		}
	})
}

func FuzzCompareVersionsTotal(f *testing.F) {
	f.Add("v1", "v2")
	f.Add("v10", "v9")
	f.Add("", "v1")
	f.Add("v01", "v1")
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := CompareVersions(a, b), CompareVersions(b, a)
		if (ab < 0) != (ba > 0) || (ab > 0) != (ba < 0) {
			t.Fatalf("not antisymmetric for %q %q: %d %d", a, b, ab, ba)
		}
		if (ab == 0) != (a == b) {
			// Equal comparison must imply equal strings, otherwise distinct
			// versions would collide in the registry.
			if a == b {
				return
			}
			t.Fatalf("distinct versions compare equal: %q %q", a, b)
		}
	})
}

func FuzzCompareVersionsTotalOrder(f *testing.F) {
	f.Add("v1", "v01", "v001")
	f.Add("v2", "v10", "v9")
	f.Add("", "v1", "v1.0")
	f.Add("a1b", "a01b", "a2b")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		// Antisymmetry.
		if ab, ba := CompareVersions(a, b), CompareVersions(b, a); ab != -ba {
			t.Fatalf("antisymmetry: %q %q: %d %d", a, b, ab, ba)
		}
		// Zero exactly on identical strings, so distinct registration keys
		// never collide.
		if (CompareVersions(a, b) == 0) != (a == b) {
			t.Fatalf("reflexivity: %q %q: %d", a, b, CompareVersions(a, b))
		}
		// Transitivity: if a<=b and b<=c then a<=c.
		ab, bc, ac := CompareVersions(a, b), CompareVersions(b, c), CompareVersions(a, c)
		if ab <= 0 && bc <= 0 && ac > 0 {
			t.Fatalf("transitivity: %q <= %q <= %q but %q > %q", a, b, c, a, c)
		}
		if ab >= 0 && bc >= 0 && ac < 0 {
			t.Fatalf("transitivity (reverse): %q >= %q >= %q but %q < %q", a, b, c, a, c)
		}
	})
}

func TestIsVersions(t *testing.T) {
	type isVersioned struct {
		S string `json:"s"`
	}
	url := fmt.Sprintf("example.com/is-versioned/%d", globalURLSeq.Add(1))
	src := Source{Name: "is-test"}
	if err := RegisterVersioned(&isVersioned{}, url, "v2", src); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = UnregisterSource(src) })

	a, err := MarshalAny(&isVersioned{S: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Is matches the versioned wire URL without the caller having to know
	// which version won the selection.
	if !Is(a, &isVersioned{}) {
		t.Fatal("Is did not match a versioned registration")
	}
	if Is(a, &unrelatedType{}) {
		t.Fatal("Is matched an unrelated type")
	}
}
