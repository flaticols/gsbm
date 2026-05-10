package inner

// ID is reused by api.Order; it lives in an internal package to
// exercise the case the old loader could not resolve without manual
// dir-list-padding (importer.Default returned no export data for
// not-yet-installed sibling packages).
type ID uint64

// Tag is treated as opaque by gsbm so the closure walker stops at the
// boundary; that also lets the marker-flow path be exercised across
// the api -> internal/inner import edge.
//
//gsbm:opaque
type Tag struct {
	Key   string
	Value string
}
