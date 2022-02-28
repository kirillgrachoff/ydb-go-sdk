// Code generated by gtrace. DO NOT EDIT.

package main

import (
	"context"
	"net/http"
)

// Compose returns a new PingTrace which has functional fields composed
// both from t and x.
func (t PingTrace) Compose(x PingTrace) (ret PingTrace) {
	switch {
	case t.OnRequest == nil:
		ret.OnRequest = x.OnRequest
	case x.OnRequest == nil:
		ret.OnRequest = t.OnRequest
	default:
		h1 := t.OnRequest
		h2 := x.OnRequest
		ret.OnRequest = func(p PingTraceRequestStart) func(PingTraceRequestDone) {
			r1 := h1(p)
			r2 := h2(p)
			switch {
			case r1 == nil:
				return r2
			case r2 == nil:
				return r1
			default:
				return func(p PingTraceRequestDone) {
					r1(p)
					r2(p)
				}
			}
		}
	}
	return ret
}

type pingTraceContextKey struct{}

// WithPingTrace returns context which has associated PingTrace with it.
func WithPingTrace(ctx context.Context, t PingTrace) context.Context {
	return context.WithValue(ctx,
		pingTraceContextKey{},
		ContextPingTrace(ctx).Compose(t),
	)
}

// ContextPingTrace returns PingTrace associated with ctx.
// If there is no PingTrace associated with ctx then zero value 
// of PingTrace is returned.
func ContextPingTrace(ctx context.Context) PingTrace {
	t, _ := ctx.Value(pingTraceContextKey{}).(PingTrace)
	return t
}

func (t PingTrace) onRequest(ctx context.Context, p PingTraceRequestStart) func(PingTraceRequestDone) {
	c := ContextPingTrace(ctx)
	var fn func(PingTraceRequestStart) func(PingTraceRequestDone) 
	switch {
	case t.OnRequest == nil:
		fn = c.OnRequest
	case c.OnRequest == nil:
		fn = t.OnRequest
	default:
		h1 := t.OnRequest
		h2 := c.OnRequest
		fn = func(p PingTraceRequestStart) func(PingTraceRequestDone) {
			r1 := h1(p)
			r2 := h2(p)
			switch {
			case r1 == nil:
				return r2
			case r2 == nil:
				return r1
			default:
				return func(p PingTraceRequestDone) {
					r1(p)
					r2(p)
				}
			}
		}
	}
	if fn == nil {
		return func(PingTraceRequestDone) {
			return
		}
	}
	res := fn(p)
	if res == nil {
		return func(PingTraceRequestDone) {
			return
		}
	}
	return res
}
func pingTraceOnRequest(ctx context.Context, t PingTrace, r *http.Request) func(*http.Response, error) {
	var p PingTraceRequestStart
	p.Request = r
	res := t.onRequest(ctx, p)
	return func(r *http.Response, e error) {
		var p PingTraceRequestDone
		p.Response = r
		p.Error = e
		res(p)
	}
}
