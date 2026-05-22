#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    echo "Copy deploy.config.example to deploy.config and edit it"
    exit 1
fi

source deploy.config

echo "readperfect Remote Setup"
echo "========================"
echo "Server: $SSH_USER@$SSH_HOST"
echo "App Home: $APP_HOME"
echo "Port: $APP_PORT"
echo "Domain: $DOMAIN"
echo "DB Path: $DB_PATH"
echo
read -p "Proceed? (y/n) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    exit 1
fi

echo "Cloning repository on server..."
ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" << EOF
    set -euo pipefail
    mkdir -p "$APP_HOME"
    cd "$APP_HOME"
    if [ -d ".git" ]; then
        git pull
    else
        git clone "$GIT_REPO" .
    fi
EOF

echo
echo "Building application on server..."
ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" << EOF
    set -euo pipefail
    cd "$APP_HOME"
    GO_BIN="${GO_PATH:-go}"
    GO_VERSION=\$("\$GO_BIN" version 2>/dev/null | awk '{print \$3}')
    if [ -z "\$GO_VERSION" ]; then
        echo "Go not found at \$GO_BIN"
        exit 1
    fi
    echo "Current Go version: \$GO_VERSION"
    "\$GO_BIN" build -o "$BINARY_NAME" .
    chmod +x "$BINARY_NAME"
EOF

echo
echo "Remote setup complete"
echo "Next steps:"
echo "  1. ./infra/remote-setup-env.sh"
echo "  2. ./infra/remote-setup-service.sh"
echo "  3. ./infra/remote-setup-nginx.sh"
