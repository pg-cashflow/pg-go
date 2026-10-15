package postgres

import (
	"context"

	"github.com/google/uuid"
)

type JoinIdempotencyRepo struct{ db DBTX }

func NewJoinIdempotencyRepo(db DBTX) *JoinIdempotencyRepo { return &JoinIdempotencyRepo{db: db} }

func (r *JoinIdempotencyRepo) Get(ctx context.Context, userID uuid.UUID, key string) ([]byte, error) {
	var body []byte
	err := r.db.QueryRow(ctx, `
		SELECT response_body FROM join_idempotency WHERE user_id=$1 AND idempotency_key=$2`,
		userID, key,
	).Scan(&body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (r *JoinIdempotencyRepo) Put(ctx context.Context, userID uuid.UUID, key string, body []byte) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO join_idempotency (user_id, idempotency_key, response_body)
		VALUES ($1,$2,$3)
		ON CONFLICT (user_id, idempotency_key) DO NOTHING`, userID, key, body)
	return err
}
