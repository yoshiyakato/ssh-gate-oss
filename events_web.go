package main

import "context"

// eventSink receives every App event when the app runs behind the Web admin
// (headless). *eventHub implements it. publish must not block and must not
// call back into App (design-v1 §5.0: the hub lock is a leaf lock).
type eventSink interface {
	publish(name string, payload any)
}

type webHubKey struct{}

// withWebHub returns a ctx that routes App events to s. runHeadless passes
// the result to app.startup, so a.ctx carries the hub without touching app.go.
func withWebHub(ctx context.Context, s eventSink) context.Context {
	return context.WithValue(ctx, webHubKey{}, s)
}

// webHubFrom returns the sink stored by withWebHub, or nil.
func webHubFrom(ctx context.Context) eventSink {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(webHubKey{}).(eventSink)
	return s
}

// wailsEmit is the Wails runtime's EventsEmit in the GUI build
// (events_wails.go sets it) and nil in the headless-only build, where Wails
// is not linked at all.
var wailsEmit func(ctx context.Context, name string, data ...any)

// platformEmit delivers an event to the Web hub in ctx if there is one, else
// to the Wails runtime. A ctx carrying a hub never reaches Wails: the Wails
// runtime log.Fatalf's on a ctx it did not create, which would kill the GUI
// binary started with --headless.
func platformEmit(ctx context.Context, name string, payload any) {
	if s := webHubFrom(ctx); s != nil {
		s.publish(name, payload)
		return
	}
	if wailsEmit != nil && ctx != nil {
		wailsEmit(ctx, name, payload)
	}
}
