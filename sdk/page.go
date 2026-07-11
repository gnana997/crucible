package crucible

// Page is one page of a list result. Today the daemon always returns
// everything in a single page and NextCursor is empty; the shape is
// pagination-ready so a future control-plane can add cursors without a
// breaking change to any list method's signature.
type Page[T any] struct {
	// Items are the results, in the daemon's order.
	Items []T

	// NextCursor is the opaque cursor for the next page, empty on the
	// last page. Reserved — always empty against a single-node daemon.
	NextCursor string
}
