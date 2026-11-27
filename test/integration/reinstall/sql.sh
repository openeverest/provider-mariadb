#!/bin/sh
# Usage: sql.sh <instance> <sql>
#
# Runs <sql> as the instance's admin user over verified TLS and prints the
# result. <sql> must not contain double quotes or backslashes.
set -eu
instance=$1
sql=$2
secret="$instance-conn"
pod="sql-$instance"

kubectl -n "$NAMESPACE" delete pod "$pod" --ignore-not-found --wait=true >/dev/null
kubectl -n "$NAMESPACE" run "$pod" \
  --rm -i --restart=Never \
  --image=mariadb:12.3 \
  --overrides="{
    \"spec\": {
      \"containers\": [{
        \"name\": \"sql\",
        \"image\": \"mariadb:12.3\",
        \"command\": [\"sh\", \"-c\"],
        \"args\": [\"mariadb -h \\\"\$DB_HOST\\\" -P \\\"\$DB_PORT\\\" -u \\\"\$DB_USER\\\" --ssl-ca=/tls/ca.crt --ssl-verify-server-cert -Nse \\\"\$SQL\\\"\"],
        \"env\": [
          {\"name\": \"SQL\", \"value\": \"$sql\"},
          {\"name\": \"DB_HOST\", \"valueFrom\": {\"secretKeyRef\": {\"name\": \"$secret\", \"key\": \"admin-host\"}}},
          {\"name\": \"DB_PORT\", \"valueFrom\": {\"secretKeyRef\": {\"name\": \"$secret\", \"key\": \"admin-port\"}}},
          {\"name\": \"DB_USER\", \"valueFrom\": {\"secretKeyRef\": {\"name\": \"$secret\", \"key\": \"admin-username\"}}},
          {\"name\": \"MYSQL_PWD\", \"valueFrom\": {\"secretKeyRef\": {\"name\": \"$secret\", \"key\": \"admin-password\"}}}
        ],
        \"volumeMounts\": [{\"name\": \"tls\", \"mountPath\": \"/tls\", \"readOnly\": true}]
      }],
      \"volumes\": [{\"name\": \"tls\", \"secret\": {\"secretName\": \"$secret\", \"items\": [{\"key\": \"admin-ca.crt\", \"path\": \"ca.crt\"}]}}]
    }
  }"
