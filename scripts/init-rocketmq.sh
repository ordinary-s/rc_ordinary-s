#!/bin/sh
set -eu
MQADMIN=/home/rocketmq/rocketmq-5.5.0/bin/mqadmin
attempt=0
while :; do
  if cluster="$("$MQADMIN" clusterList -n "$NAMESRV_ADDR" 2>/dev/null)"; then
    case "$cluster" in
      *notification-broker*) break ;;
    esac
  fi
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 90 ]; then
    echo "RocketMQ broker route was not registered within 90 seconds" >&2
    exit 1
  fi
  sleep 1
done
"$MQADMIN" updateTopic -n "$NAMESRV_ADDR" -c DefaultCluster -t "$ROCKETMQ_TOPIC" -r 8 -w 8
"$MQADMIN" updateTopic -n "$NAMESRV_ADDR" -c DefaultCluster -t "%DLQ%$ROCKETMQ_CONSUMER_GROUP" -r 8 -w 8
"$MQADMIN" updateSubGroup -n "$NAMESRV_ADDR" -c DefaultCluster -g "$ROCKETMQ_CONSUMER_GROUP" -m true -q 8 -r 8
"$MQADMIN" updateSubGroup -n "$NAMESRV_ADDR" -c DefaultCluster -g notification_dlq_audit -m true -q 8 -r 8
"$MQADMIN" updateSubGroup -n "$NAMESRV_ADDR" -c DefaultCluster -g notification_retry_audit -m true -q 8 -r 8
echo "RocketMQ topic and consumer group initialized"
