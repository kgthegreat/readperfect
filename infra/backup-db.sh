#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

TIMESTAMP=$(date +%Y%m%d_%H%M%S)
LOCAL_BACKUP="./backups/readperfect-$TIMESTAMP.db"

echo "Backing up database from $SSH_HOST"
echo "Remote DB: $APP_HOME/$DB_PATH"
echo "Local backup: $LOCAL_BACKUP"

mkdir -p ./backups
scp -P "$SSH_PORT" "$SSH_USER@$SSH_HOST:$APP_HOME/$DB_PATH" "$LOCAL_BACKUP"

echo "Backup complete"
du -h "$LOCAL_BACKUP"
echo
ls -lht ./backups/ | head -5
