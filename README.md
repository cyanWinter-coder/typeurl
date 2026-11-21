# typeurl

[![PkgGoDev](https://pkg.go.dev/badge/github.com/containerd/typeurl)](https://pkg.go.dev/github.com/containerd/typeurl)
[![Build Status](https://github.com/containerd/typeurl/workflows/CI/badge.svg)](https://github.com/containerd/typeurl/actions?query=workflow%3ACI)
[![codecov](https://codecov.io/gh/containerd/typeurl/branch/main/graph/badge.svg)](https://codecov.io/gh/containerd/typeurl)
[![Go Report Card](https://goreportcard.com/badge/github.com/containerd/typeurl)](https://goreportcard.com/report/github.com/containerd/typeurl)

A Go package for managing the registration, marshaling, and unmarshaling of encoded types.

This package helps when types are sent over a ttrpc/GRPC API and marshaled as a protobuf [Any](https://pkg.go.dev/google.golang.org/protobuf@v1.27.1/types/known/anypb#Any)

## Project details

**typeurl** is a containerd sub-project, licensed under the [Apache 2.0 license](./LICENSE).
As a containerd sub-project, you will find the:
 * [Project governance](https://github.com/containerd/project/blob/main/GOVERNANCE.md),
 * [Maintainers](https://github.com/containerd/project/blob/main/MAINTAINERS),
 * and [Contributing guidelines](https://github.com/containerd/project/blob/main/CONTRIBUTING.md)

information in our [`containerd/project`](https://github.com/containerd/project) repository.

## Versioned registries

The package-level `Register`/`UnmarshalAny` functions keep a single global
mapping from type URL to Go type. When a plugin is hot-reloaded and a new Go
type is registered for the same type URL, the last registration wins and the
decoded result depends on load order.

`Registry` and `Snapshot` provide an opt-in, explicit alternative:

```go
reg := typeurl.NewRegistry("my-plugin")

err := reg.Register(typeurl.Entry{
    TypeURL: "types.example.com/greeting",
    Version: "v2",
    GoType:  reflect.TypeOf(greetingV2{}),
    Source:  typeurl.Source{Name: "my-plugin", Detail: "build-42"},
})

snapshot := reg.Snapshot()                       // immutable, cached
anyGreeting, err := snapshot.MarshalAny(msg)     // pins this snapshot
msg, err := snapshot.UnmarshalAny(anyGreeting)
reg.SetSnapshot(newSnapshot)                     // atomic switch
```

* Each entry records its type URL, protocol version, Go type and `Source`.
  Registering the same type URL and version twice returns `ErrConflict`.
* Versions are encoded in the Any type URL as `base#version` (path-escaped).
  The default version adds no suffix, so existing type URLs are unchanged.
  A message-declared version resolves exactly; otherwise the registry's
  selector (default: unversioned first, then highest version) decides.
* `Freeze` stops registration churn; `Snapshot` publishes an immutable,
  shared view that `MarshalAny`/`UnmarshalAny` use as a single consistent
  revision until `SetSnapshot` swaps it atomically.
* `UnregisterSource` removes exactly the entries a plugin contributed, so a
  reloaded plugin does not leave stale registrations behind.
* Unknown URLs return `ErrNotFound`; version and type mismatches return
  `ErrVersionMismatch` and `ErrTypeMismatch`.

All of the above is safe for concurrent use, and the legacy package-level
functions keep their existing behavior: they consult the original global
table first, then the default versioned registry, then the protobuf global
type registry.

## Gogo Protobuf Support Deprecation

Support for gogoprotobuf was removed in v2.3.0. The upstream package has been deprecated since 2022 and users of
typeurl should not rely on Gogo Protobuf support anymore. Users which are still transitioning away from it may
continue to use the v2.2 release until that transition is complete. Since v2.2.1, gogo proto support can be
explicitly removed using the `!no_gogo` build tag.
