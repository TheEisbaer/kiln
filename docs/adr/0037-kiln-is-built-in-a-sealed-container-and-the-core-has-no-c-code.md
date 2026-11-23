---
status: accepted
---

# Kiln is built in a sealed container, and the core contains no C code

A new version of Kiln (ADR 0019) is compiled inside a sealed container started from Kiln's toolchain image, `images/toolchain`, the one the checks build in, and the result is a program for the developer's machine. The first install is the same build, started by one command. The developer's machine therefore needs only the container engine; the toolchain is the one the checks used, pinned and changed by a card; and the install scripts of UI dependencies run sealed, with only the listed hosts reachable, not on the developer's machine with their home directory in reach.

The price is a permanent restriction: the core contains no C code, because a program for macOS with C parts cannot reasonably be built from a Linux container. That narrows the choice of embedded database (ADR 0032) to ones written in pure Go. Confirmed on 2026-11-16: a Go program compiled in a Linux container for macOS on arm64 ran on the developer's machine, signed by the compiler itself, with name lookup working.

## Considered options

- **On the developer's machine, with Go and Node installed there**: the smallest and fastest, with no restriction on what the core links. Not chosen because Node's version would be whatever is installed, dependency install scripts would run unsealed, and each colleague would have to install matching toolchains. It is not kept as a second path. If building in a container stops working, for example because Kiln truly needs a C dependency, Kiln switches to this by a decision.
- **By the checks runner after the merge**: built once by the thing that tested it, but the result would have to reach the core through a shared directory, since nothing may be stored on the Git host (ADR 0026), and updating would depend on Actions.
- **Dagger for the build**: considered for building for several architectures. Not needed, because Go builds for another system by itself and every target in the first setup is arm64 (ADR 0003); it would also not lift the restriction above, and its engine is a privileged container whose builds are not behind Kiln's proxy.

## Consequences

- Go code compiled into the core still runs on the developer's machine; what is sealed is the build.
- Images for more than one architecture are a matter for a public Kiln and are decided then.
