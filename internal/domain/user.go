package domain

import (
	"errors"
	"time"
)

var (
	ErrInvalidUser   = errors.New("invalid user")
	ErrUsernameTaken = errors.New("username already exists")
)

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}
