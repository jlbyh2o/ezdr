package api

import "errors"

var (
	errTooManyAttempts = errors.New("too many attempts; wait a moment and try again")
	errNotSignedIn     = errors.New("not signed in")
	errBadCredentials  = errors.New("invalid username or password")
	errBadCode         = errors.New("invalid or expired code")
)
