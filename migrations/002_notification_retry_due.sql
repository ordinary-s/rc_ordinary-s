ALTER TABLE notification
	ADD COLUMN next_attempt_at DATETIME(6) NULL AFTER expires_at;
