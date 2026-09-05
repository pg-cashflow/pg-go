package domain

import "github.com/google/uuid"

// Ptr returns a pointer to id (for nullable FK / event fields).
func Ptr(id uuid.UUID) *uuid.UUID { return &id }

// Int16Ptr returns a pointer to v.
func Int16Ptr(v int16) *int16 { return &v }
