package maintenance

// Notifier receives the single final record of every request. Notify is called
// at most once per request: completion, abort and timeout all flow through the
// same finalizer, so even when they race there is only one notification.
type Notifier interface {
	Notify(rec Record)
}

// NotifierFunc adapts a plain function into a Notifier.
type NotifierFunc func(rec Record)

// Notify implements Notifier.
func (f NotifierFunc) Notify(rec Record) {
	f(rec)
}
