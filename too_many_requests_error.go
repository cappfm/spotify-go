package spotify

import (
	"fmt"
	"time"
)

type TooManyRequestsError struct {
	RetryAfter time.Duration
}

func (t TooManyRequestsError) Error() string {
	return fmt.Sprintf("spotify: too many request: retry after %s", t.RetryAfter)
}
