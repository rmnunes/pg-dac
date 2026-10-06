-- Example declarative project: the desired schema as the source of truth.
CREATE SCHEMA app;

CREATE TYPE app.status AS ENUM ('active', 'inactive', 'pending');

CREATE TABLE app.users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL,
    status     app.status NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_uniq UNIQUE (email)
);

CREATE TABLE app.orders (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES app.users (id),
    amount  numeric(12, 2) NOT NULL CHECK (amount >= 0)
);

CREATE INDEX orders_user_id_idx ON app.orders (user_id);

CREATE VIEW app.active_users AS
    SELECT id, email FROM app.users WHERE status = 'active';
