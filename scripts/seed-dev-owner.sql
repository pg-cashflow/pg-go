-- Seed Firebase test phone as a property owner so POST /auth/firebase can issue a JWT.
-- Firebase Console test number: +918008281429 / OTP 123456
-- Idempotent: inserts only if no property already has this owner_phone.

INSERT INTO properties (name, address, owner_phone, upi_vpa, owner_name, owner_email)
SELECT
  'Dev PG',
  'Local development',
  '+918008281429',
  'dev@upi',
  'Dev Owner',
  'dev@example.com'
WHERE NOT EXISTS (
  SELECT 1 FROM properties WHERE owner_phone = '+918008281429'
);
