#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "readperfect Status"
echo "=================="
echo
echo "Server: $SSH_USER@$SSH_HOST"
echo
echo "Service Status:"
ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "sudo systemctl status $SERVICE_NAME --no-pager | head -n 10"

echo
echo "HTTP Check:"
curl -Is "https://$DOMAIN" | head -n 1 || echo "Site not reachable"

echo
echo "Disk Usage (App Home):"
ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "df -h $APP_HOME | tail -n 1"

echo
echo "Database File:"
ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "ls -lh $APP_HOME/$DB_PATH 2>/dev/null || echo 'Database not created yet'"

echo
echo "Last Deployment:"
ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "cd $APP_HOME && git log -1 --format='%h - %s (%cr)'"
