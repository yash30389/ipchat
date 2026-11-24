-- Persistent member identities
CREATE TABLE IF NOT EXISTS room_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    room_code VARCHAR(32) NOT NULL
        REFERENCES rooms(room_code) ON DELETE CASCADE,
    username VARCHAR(50) NOT NULL,
    identity_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT unique_room_username
        UNIQUE (room_code, username)
);

-- Message delivery and editing metadata
ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS message_type VARCHAR(20)
        NOT NULL DEFAULT 'text';

ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS edited_at TIMESTAMPTZ;

-- Emoji reactions
CREATE TABLE IF NOT EXISTS message_reactions (
    id BIGSERIAL PRIMARY KEY,
    message_id BIGINT NOT NULL
        REFERENCES messages(id) ON DELETE CASCADE,
    member_id UUID NOT NULL
        REFERENCES room_members(id) ON DELETE CASCADE,
    emoji VARCHAR(16) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (message_id, member_id, emoji)
);

-- Message lookup and history performance
CREATE INDEX IF NOT EXISTS idx_messages_room_created
    ON messages(room_code, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_room_members_room
    ON room_members(room_code);