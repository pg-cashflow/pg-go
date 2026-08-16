-- Link Firebase Auth UID to the existing phone-keyed users row.
ALTER TABLE users
  ADD COLUMN firebase_uid TEXT;

CREATE UNIQUE INDEX users_firebase_uid_unique
  ON users(firebase_uid)
  WHERE firebase_uid IS NOT NULL;
