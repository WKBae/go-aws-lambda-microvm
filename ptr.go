package microvm

func ptr[T any](v T) *T {
	// ptr(v) can be replaced with new(v) in Go 1.26
	return &v
}
