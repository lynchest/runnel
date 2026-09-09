## 2025-05-18 - Avoid Request Cloning and Header Loop Lowercasing in Proxy Hot Paths
**Learning:** Calling `r.Clone(ctx)` to temporarily mutate request fields (such as `r.URL`) triggers deep copies of the `http.Header` map and all header slice values on every request. Additionally, nested loops over standard hop-by-hop headers called `strings.ToLower` on constant string slices repeatedly for each header in each request, and eagerly allocated token maps even when no `Connection` header was present.
**Action:** Accept target URLs directly in fingerprint calculations to eliminate `r.Clone` allocations, and use pre-lowercased package-level lookup maps with lazy token map allocation for header filtering.

## 2026-09-09 - Pre-normalize Default Config Lists and Avoid r.Cookies() Heap Allocations
**Learning:** Package-default slice configurations (such as default cookie lists) cause repeated map deduplication and sorting overhead when normalized per request. Furthermore, calling `r.Cookies()` constructs heap-allocated `[]*http.Cookie` structs, whereas checking if `r.Header["Cookie"]` is present and parsing header lines directly avoids heap allocations on fingerprint hot paths.
**Action:** Pre-normalize static default config lists at package initialization, bypass cookie parsing completely when no `Cookie` header exists, and pre-grow `strings.Builder` capacity in concatenation helpers.
