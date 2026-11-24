# Encryption Design

Message text is encrypted in the browser with AES-256-GCM. Each operation uses a unique 12-byte random nonce. An envelope includes version, algorithm, room-code AAD, nonce, and ciphertext.

The room passphrase is sent to the backend for room authorization only. It is never used as an encryption key. The browser derives a 256-bit key with PBKDF2-SHA256, 250,000 iterations, and a random 16-byte salt stored in the rooms table. PBKDF2 is used only because Web Crypto does not expose Argon2id in browsers; this is not a claim that password/passphrase KDF requirements are fully Argon2id-compliant.

PostgreSQL stores only the JSON envelope and cannot decrypt it. Key material is not persisted or logged. New devices need the room passphrase and room salt; lost passphrases make existing history unrecoverable. Server deletion removes server copies but cannot erase copies already downloaded or cached on clients.
