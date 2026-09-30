-- 000002_create_users.sql
-- Accounts that can log in. Passwords are NEVER stored, only bcrypt hashes.

CREATE TABLE users (
    -- BIGINT identity: the database generates 1, 2, 3, ...
    -- GENERATED ALWAYS forbids callers from inserting their own ids by accident.
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- CITEXT (from migration 000001) compares case-insensitively, so the
    -- UNIQUE constraint below treats 'Bob@x.com' and 'bob@x.com' as equal.
    email         CITEXT      NOT NULL,
    password_hash TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    role          TEXT        NOT NULL DEFAULT 'CUSTOMER',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One account per email. This constraint (not a SELECT before INSERT) is
    -- what stops two simultaneous registrations for the same email: the
    -- database lets only one INSERT win. The name is referenced from Go code
    -- to recognise this specific violation.
    CONSTRAINT users_email_key UNIQUE (email),

    -- Only known roles. Even a bug or a manual SQL typo cannot create a
    -- 'SUPERADMIN' that the application does not understand.
    CONSTRAINT users_role_check CHECK (role IN ('CUSTOMER', 'ADMIN')),

    -- Defence in depth: the API validates these too, but the database is the
    -- last line of defence for any code path (scripts, future services, psql).
    CONSTRAINT users_email_length_check CHECK (char_length(email::text) BETWEEN 3 AND 254),
    CONSTRAINT users_name_not_blank_check CHECK (btrim(name) <> ''),
    CONSTRAINT users_password_hash_not_blank_check CHECK (password_hash <> '')
);

-- No extra index on email is needed: the UNIQUE constraint already creates
-- one, and it is what makes "find user by email" at login fast.
