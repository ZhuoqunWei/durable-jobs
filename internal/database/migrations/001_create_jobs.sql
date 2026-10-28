CREATE TABLE jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    type text NOT NULL CHECK (type = 'migrate_campaign'),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    status text NOT NULL DEFAULT 'queued' CHECK (status = 'queued'),
    created_at timestamptz NOT NULL DEFAULT now()
);
