CREATE TABLE api_signature_nonce (
  key_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  nonce VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  expires_at DATETIME(6) NOT NULL,
  PRIMARY KEY (key_id, nonce),
  KEY ix_api_signature_nonce_expires_at (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
