// Package singleflight is a typed, process-local fill coalescer for proxy cold
// misses: concurrent calls for the same key run the fill function once and all
// share its result. It is a thin generic wrapper over
// golang.org/x/sync/singleflight.
//
// Coalescing is per process. Different replicas may each fetch the same key
// concurrently — that is acceptable for a pull-through cache.
package singleflight

import xsf "golang.org/x/sync/singleflight"

// Group coalesces concurrent fills that produce a value of type T.
type Group[T any] struct {
	g xsf.Group
}

// Do runs fn for key, collapsing concurrent calls for the same key into a
// single execution whose result is shared by all callers. shared reports
// whether the result was shared with other in-flight callers.
func (gr *Group[T]) Do(key string, fn func() (T, error)) (value T, err error, shared bool) {
	v, err, shared := gr.g.Do(key, func() (any, error) {
		return fn()
	})
	if err != nil {
		var zero T
		return zero, err, shared
	}
	return v.(T), nil, shared
}

// Forget drops key from the in-flight set so the next Do for it runs fn afresh
// rather than joining an in-flight call.
func (gr *Group[T]) Forget(key string) { gr.g.Forget(key) }
