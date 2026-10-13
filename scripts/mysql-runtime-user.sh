#!/bin/sh
set -eu
mysql -uroot -p"$MYSQL_ROOT_PASSWORD" --execute="REVOKE ALL PRIVILEGES ON notifications.* FROM 'notification_app'@'%'; GRANT SELECT, INSERT, UPDATE, DELETE ON notifications.* TO 'notification_app'@'%';"
