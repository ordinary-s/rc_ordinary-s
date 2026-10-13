CREATE TABLE IF NOT EXISTS destination (
	destination_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
	target_url VARCHAR(2048) NOT NULL,
	fast_fail_enabled BOOLEAN NOT NULL DEFAULT FALSE,
	updated_by VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NULL,
	update_reason VARCHAR(500) NULL,
	updated_at DATETIME(6) NOT NULL,
	PRIMARY KEY (destination_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE IF NOT EXISTS notification (
	task_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	client_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
	idempotency_key VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
	request_fingerprint BINARY(32) NOT NULL,
	destination_id VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
	http_method VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	request_headers JSON NOT NULL,
	request_body MEDIUMBLOB NOT NULL,
	trace_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	http_attempts TINYINT UNSIGNED NOT NULL DEFAULT 0,
	result_reason VARCHAR(80) CHARACTER SET ascii COLLATE ascii_bin NULL,
	http_status SMALLINT UNSIGNED NULL,
	created_at DATETIME(6) NOT NULL,
	updated_at DATETIME(6) NOT NULL,
	expires_at DATETIME(6) NOT NULL,
	resolved_by VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NULL,
	resolution VARCHAR(1000) NULL,
	resolved_at DATETIME(6) NULL,
	PRIMARY KEY (task_id),
	UNIQUE KEY uq_notification_client_idempotency (client_id, idempotency_key),
	KEY idx_notification_client_created (client_id, created_at, task_id),
	KEY idx_notification_client_status_created (client_id, status, created_at, task_id),
	KEY idx_notification_status_expiry (status, expires_at),
	CONSTRAINT fk_notification_destination FOREIGN KEY (destination_id) REFERENCES destination (destination_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE IF NOT EXISTS delivery_attempt (
	task_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	attempt_no TINYINT UNSIGNED NOT NULL,
	state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	http_status SMALLINT UNSIGNED NULL,
	error_summary VARCHAR(200) NULL,
	started_at DATETIME(6) NOT NULL,
	finished_at DATETIME(6) NULL,
	PRIMARY KEY (task_id, attempt_no),
	KEY idx_delivery_attempt_state_started (state, started_at),
	CONSTRAINT fk_attempt_notification FOREIGN KEY (task_id) REFERENCES notification (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE IF NOT EXISTS outbox (
	outbox_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
	event_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	task_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	trace_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	publish_state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	publish_attempts INT UNSIGNED NOT NULL DEFAULT 0,
	next_attempt_at DATETIME(6) NOT NULL,
	published_at DATETIME(6) NULL,
	last_error VARCHAR(200) NULL,
	created_at DATETIME(6) NOT NULL,
	PRIMARY KEY (outbox_id),
	UNIQUE KEY uq_outbox_event_id (event_id),
	UNIQUE KEY uq_outbox_task_id (task_id),
	KEY idx_outbox_pending_due (publish_state, next_attempt_at, outbox_id),
	CONSTRAINT fk_outbox_notification FOREIGN KEY (task_id) REFERENCES notification (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
