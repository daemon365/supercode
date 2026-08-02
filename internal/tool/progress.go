package tool

import "context"

// Progress is an incremental observation produced while a tool is still
// running. Delta is append-only text; callers should render it as untrusted
// terminal output.
type Progress struct {
	Delta     string
	SessionID int64
}

type progressReporter func(Progress)
type progressContextKey struct{}

// WithProgressReporter installs the agent-owned progress sink for one tool
// invocation. The context scope prevents background processes from retaining
// a stale UI callback after that invocation returns.
func WithProgressReporter(ctx context.Context, reporter func(Progress)) context.Context {
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, progressContextKey{}, progressReporter(reporter))
}

// ReportProgress emits a bounded incremental observation from any tool. It is
// a no-op when the caller is not running under an Agent progress context.
func ReportProgress(ctx context.Context, progress Progress) {
	if ctx == nil || progress.Delta == "" {
		return
	}
	reporter, _ := ctx.Value(progressContextKey{}).(progressReporter)
	if reporter != nil {
		reporter(progress)
	}
}
