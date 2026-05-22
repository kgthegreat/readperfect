#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "Rolling back readperfect on $SSH_HOST"
echo "====================================="

BACKUPS=$(ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "cd $APP_HOME && ls -t ${BINARY_NAME}.backup.* 2>/dev/null | head -5")

if [ -z "$BACKUPS" ]; then
    echo "No backups found on server"
    exit 1
fi

echo "Available backups:"
echo "$BACKUPS" | nl
echo
read -p "Select backup number to restore (1-5): " BACKUP_NUM

SELECTED=$(echo "$BACKUPS" | sed -n "${BACKUP_NUM}p")

if [ -z "$SELECTED" ]; then
    echo "Invalid selection"
    exit 1
fi

echo "Rolling back to: $SELECTED"
read -p "Confirm? (y/n) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    exit 1
fi

ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "
    cd $APP_HOME && \
    cp $SELECTED $BINARY_NAME && \
    chmod +x $BINARY_NAME && \
    sudo systemctl restart $SERVICE_NAME && \
    sleep 2 && \
    sudo systemctl status $SERVICE_NAME --no-pager | head -n 15
"

echo "Rollback complete"
