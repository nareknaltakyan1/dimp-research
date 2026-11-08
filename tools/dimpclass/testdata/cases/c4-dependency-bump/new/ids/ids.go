package ids

import "github.com/google/uuid"

// NewRequestID returns a random request id.
func NewRequestID() string { return uuid.NewString() }
