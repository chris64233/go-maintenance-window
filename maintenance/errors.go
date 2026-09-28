package maintenance

import "errors"

// Sentinel errors returned by the engine. Callers can inspect them with
// errors.Is.
var (
	// ErrResourceExists: a resource with the same id is already registered.
	ErrResourceExists = errors.New("maintenance: resource already registered")
	// ErrResourceNotFound: a referenced resource does not exist.
	ErrResourceNotFound = errors.New("maintenance: resource not found")
	// ErrRequestExists: a request with the same id already exists.
	ErrRequestExists = errors.New("maintenance: request already exists")
	// ErrRequestNotFound: the request does not exist.
	ErrRequestNotFound = errors.New("maintenance: request not found")
	// ErrInvalidWindow: the time window is invalid (non-positive duration,
	// already elapsed, ...).
	ErrInvalidWindow = errors.New("maintenance: invalid time window")
	// ErrWindowOverlap: the window overlaps another non-final request on a
	// shared resource.
	ErrWindowOverlap = errors.New("maintenance: window overlaps an existing request")
	// ErrInvalidState: the request is not in a state that allows the
	// requested transition.
	ErrInvalidState = errors.New("maintenance: request is not in a valid state for this operation")
	// ErrResourceBusy: a resource needed for starting is currently held by
	// another running request.
	ErrResourceBusy = errors.New("maintenance: resource is occupied by another request")
	// ErrUnknownStep: a receipt names a step that is not required by the
	// current version of the request.
	ErrUnknownStep = errors.New("maintenance: unknown preparation step")
	// ErrTooEarly: the window has not reached its scheduled start time.
	ErrTooEarly = errors.New("maintenance: window has not started yet")
)
