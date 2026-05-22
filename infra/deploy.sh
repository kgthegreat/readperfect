#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "Deploying readperfect to $SSH_HOST"
echo "=================================="

if git status --porcelain | grep -q '^'; then
    echo "You have uncommitted changes"
    read -p "Continue anyway? (y/n) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

CURRENT_BRANCH=$(git branch --show-current)
echo "Current branch: $CURRENT_BRANCH"
echo

echo "Pulling latest code on server..."
ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" << EOF
    set -euo pipefail
    cd "$APP_HOME"
    git fetch origin
    git pull origin "$CURRENT_BRANCH"
EOF

echo
echo "Building on server..."
ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" << EOF
    set -euo pipefail
    cd "$APP_HOME"
    GO_BIN="${GO_PATH:-go}"
    "\$GO_BIN" build -o "${BINARY_NAME}.new" .
    if [ -f "$BINARY_NAME" ]; then
        cp "$BINARY_NAME" "$BINARY_NAME.backup.\$(date +%s)"
    fi
    mv "${BINARY_NAME}.new" "$BINARY_NAME"
    chmod +x "$BINARY_NAME"
EOF

echo
echo "Restarting service..."
ssh -t -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "sudo systemctl restart $SERVICE_NAME && sleep 2 && sudo systemctl status $SERVICE_NAME --no-pager | head -n 15"

echo
echo "Deployment complete"
echo "Check: https://$DOMAIN"
