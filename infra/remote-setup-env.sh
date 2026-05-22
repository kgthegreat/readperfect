#!/bin/bash
set -euo pipefail

if [ ! -f "deploy.config" ]; then
    echo "deploy.config not found"
    exit 1
fi

source deploy.config

echo "readperfect Environment Configuration"
echo "====================================="
echo

read -p "Bootstrap admin email (optional): " BOOTSTRAP_ADMIN_EMAIL
read -p "Google Client ID: " GOOGLE_CLIENT_ID
read -p "Google Client Secret: " GOOGLE_CLIENT_SECRET

GOOGLE_REDIRECT_URL="https://$DOMAIN/auth/google/callback"

echo
echo "Creating .env on server..."

ssh -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" << EOF
    set -euo pipefail
    cd "$APP_HOME"

    if [ -f ".env" ]; then
        cp .env ".env.backup.\$(date +%s)"
    fi

cat > .env << 'ENVEOF'
PORT=$APP_PORT
DATABASE_PATH=$DB_PATH
COOKIE_SECURE=true
BOOTSTRAP_ADMIN_EMAIL=$BOOTSTRAP_ADMIN_EMAIL
GOOGLE_CLIENT_ID=$GOOGLE_CLIENT_ID
GOOGLE_CLIENT_SECRET=$GOOGLE_CLIENT_SECRET
GOOGLE_REDIRECT_URL=$GOOGLE_REDIRECT_URL
GOOGLE_LOGIN_ENABLED=false
ENVEOF

    chmod 600 .env
EOF

echo "Environment configured on server"
