module github.com/promise-language/flow/tools/build

go 1.26

// The shared helpers every managed project's tooling needs — hash, staleness,
// platform, exec, args, hook wiring — live once in forge/primitives rather
// than as a copy here (forge docs/primitives.md §1). The dependency is pinned
// at an exact version and verified by go.sum, so an upstream change reaches
// this project when this line is raised and never before (§3).
require github.com/promise-language/forge v0.0.0-20260910142522-d5f6db37e698
