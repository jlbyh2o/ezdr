package auth

import (
	"crypto/rand"
	"sync"
	"time"
)

// ChallengeTTL is how long a user has to complete the TOTP step.
const ChallengeTTL = 5 * time.Minute

// maxChallengeAttempts limits TOTP guesses per challenge.
const maxChallengeAttempts = 5

// Challenge is a sign-in in progress: the password was correct and the TOTP
// step remains.
type Challenge struct {
	UserID string
	// PendingSecret is set when the user is setting up TOTP for the first
	// time; it is saved only after a valid code proves the app is set up.
	PendingSecret string
	expires       time.Time
	attempts      int
}

// Challenges holds sign-in challenges in memory. They are short-lived, so
// losing them on restart only means signing in again.
type Challenges struct {
	mu sync.Mutex
	m  map[string]*Challenge
}

// NewChallenges returns an empty challenge store.
func NewChallenges() *Challenges {
	return &Challenges{m: make(map[string]*Challenge)}
}

// Create stores a challenge and returns its ID.
func (c *Challenges) Create(ch Challenge) string {
	id := rand.Text()
	ch.expires = time.Now().Add(ChallengeTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.m {
		if time.Now().After(v.expires) {
			delete(c.m, k)
		}
	}
	c.m[id] = &ch
	return id
}

// Attempt returns the challenge for id and counts an attempt. It returns
// false if the challenge does not exist, has expired, or has no attempts
// left, in which case it is removed.
func (c *Challenges) Attempt(id string) (Challenge, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.m[id]
	if !ok {
		return Challenge{}, false
	}
	ch.attempts++
	if time.Now().After(ch.expires) || ch.attempts > maxChallengeAttempts {
		delete(c.m, id)
		return Challenge{}, false
	}
	return *ch, true
}

// Delete removes a challenge after successful use.
func (c *Challenges) Delete(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}
