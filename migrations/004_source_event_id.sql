ALTER TABLE notification
	ADD COLUMN source_event_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NULL AFTER idempotency_key,
	ADD UNIQUE KEY uq_notification_client_source_event (client_id, source_event_id);
