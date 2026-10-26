package idempotency

import "errors"

var ErrConflict = errors.New("idempotency request conflicts with existing key")

type Claim struct {
	ID               int64
	Result           []byte
	Acquired         bool
	Completed        bool
	Pending          bool
	RecoveryRequired bool
}
